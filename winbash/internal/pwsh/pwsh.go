// Package pwsh 管理 ② 层的执行引擎:一个常驻的 pwsh 子进程。
//
// 它存在的唯一理由是「不等待」:pwsh 冷启一次约 350ms,而 ② 层的每条命令(reg/svc/ps/sys)
// 都要经过 PowerShell 才能碰到 Windows 的真实能力。把它常驻下来,一次往返约 1–2ms,
// 与 winbash 自己的 daemon 是同一套思路,只是下移了一层。
//
// 与 pwshx 的关系:协议(4 字节小端长度 + UTF-8 JSON)、-EncodedCommand 启动方式、
// UTF-16LE 编码都在那边验证过,这里沿用。差别只有一处 —— pwshx 用命名管道是因为它的
// client 是独立进程;这里的调用方就是持有子进程的 winbash daemon,直接走 stdin/stdout
// 更短也更不容易出错(命名管道还要处理 \\.\pipe\ 前缀与 ERROR_PIPE_BUSY)。
package pwsh

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf16"

	_ "embed"
)

//go:embed session.ps1
var sessionScript string

const (
	createNoWindow      = 0x08000000
	createNewProcessGrp = 0x00000200

	// startBudget 是「拉起 pwsh 并完成一次握手」的预算。冷启约 350ms,
	// 但在繁忙机器上会明显更长 —— 这里宁可等久一点,也不要误判成启动失败。
	startBudget = 30 * time.Second

	// maxFrameBytes 是响应的上限。超过它说明帧已经失步(例如脚本往主机流写了东西),
	// 必须报错而不是把垃圾当数据交给上层。
	maxFrameBytes = 64 << 20

	// closeBudget 是关闭时等待 pwsh 自行退出的时间,超时就强杀。
	// 走到「等」这一步只可能是 pwsh 正卡在一条长命令里 —— 它那时根本不读 stdin,
	// 所以这个值不需要大:正常关闭是读到 EOF 后立刻 exit 0,百毫秒级。
	closeBudget = 2 * time.Second

	// stderrKeep 是保留多少 stderr 作为排错线索(pwsh 自己没起来时的信息都在这里)。
	stderrKeep = 8 << 10
)

type request struct {
	Op     string `json:"op"`
	Script string `json:"script,omitempty"`
	Cwd    string `json:"cwd,omitempty"`
}

type meta struct {
	Pid   int   `json:"pid"`
	Calls int   `json:"calls"`
	Ms    int64 `json:"ms"`
}

type response struct {
	OK     bool     `json:"ok"`
	Out    string   `json:"out"`
	Errors []string `json:"errors"`
	Meta   meta     `json:"meta"`
}

// Conn 是一个常驻 pwsh 会话。
//
// 调用面是串行的:Run 内部加锁,因为协议本身就是「一问一答」,
// 两个 goroutine 同时写同一根管道只会让帧交错。
type Conn struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	reader *bufio.Reader
	stderr *tailBuffer

	mu    sync.Mutex
	calls int

	// dead 用原子布尔而不是靠 mu:call 在超时路径上要标记失效并断开连接,
	// 而那时它**正持有** mu —— 若 Close 也要 mu,就是自己等自己(死锁)。
	dead atomic.Bool
	once sync.Once
}

// Start 拉起常驻 pwsh 并完成一次握手。
//
// 握手(ping)是启动的一部分,不是可选步骤:pwsh 路径写错、ExecutionPolicy 挡住脚本、
// 权限不足 —— 这些失败在 cmd.Start() 时都看不出来,只有真的问一句才知道。
// 让 Start 返回错误,调用方就不必为「一个永远不会回话的引擎」写兜底。
func Start() (*Conn, error) {
	cmd := exec.Command(findPwsh(),
		"-NoProfile", "-NoLogo", "-NonInteractive",
		"-ExecutionPolicy", "Bypass",
		"-EncodedCommand", base64.StdEncoding.EncodeToString(utf16LE(sessionScript)),
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow | createNewProcessGrp,
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("pwsh: 建立 stdin 管道: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("pwsh: 建立 stdout 管道: %w", err)
	}
	// pwsh 子进程没有窗口,stderr 若直接丢弃,启动失败就是完全静默的。
	// 留最后几 KB 作为线索,出错时附在错误信息里。
	tail := &tailBuffer{limit: stderrKeep}
	cmd.Stderr = tail

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("pwsh: 启动 %s 失败: %w", cmd.Path, err)
	}

	c := &Conn{
		cmd:    cmd,
		stdin:  stdin,
		reader: bufio.NewReaderSize(stdout, 64<<10),
		stderr: tail,
	}

	ctx, cancel := context.WithTimeout(context.Background(), startBudget)
	defer cancel()
	if _, err := c.call(ctx, request{Op: "ping"}); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("pwsh: 握手失败(引擎没能起来): %w%s", err, tail.hint())
	}
	return c, nil
}

// Run 在常驻会话里执行一段脚本,返回它序列化成 JSON 的文本(见 session.ps1)。
//
// 脚本里出现任何 PowerShell 错误(终止性或非终止性)都返回 error ——
// 这是 spec §7 第 1 条要求修掉的坑:PowerShell 的 non-terminating error 默认不改退出码,
// 上层会以为成功了。
func (c *Conn) Run(ctx context.Context, script string) (string, error) {
	resp, err := c.call(ctx, request{Op: "run", Script: script})
	if err != nil {
		return "", err
	}
	return resp.Out, nil
}

// call 发一个请求收一个响应。串行化 + 帧校验都在这里。
func (c *Conn) call(ctx context.Context, req request) (response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.dead.Load() {
		return response{}, errors.New("pwsh: 会话已失效(上一次出错或超时),需要重新启动")
	}

	body, err := json.Marshal(req)
	if err != nil {
		return response{}, fmt.Errorf("pwsh: 编码请求: %w", err)
	}
	if err := writeFrame(c.stdin, body); err != nil {
		c.dead.Store(true)
		return response{}, fmt.Errorf("pwsh: 写请求失败: %w%s", err, c.stderr.hint())
	}

	type frameResult struct {
		body []byte
		err  error
	}
	ch := make(chan frameResult, 1)
	go func() {
		b, err := readFrame(c.reader)
		ch <- frameResult{b, err}
	}()

	select {
	case r := <-ch:
		if r.err != nil {
			c.dead.Store(true)
			return response{}, fmt.Errorf("pwsh: 读响应失败: %w%s", r.err, c.stderr.hint())
		}
		var resp response
		if err := json.Unmarshal(r.body, &resp); err != nil {
			c.dead.Store(true)
			return response{}, fmt.Errorf("pwsh: 响应不是合法 JSON(帧已失步?): %w", err)
		}
		c.calls++
		if !resp.OK {
			return resp, fmt.Errorf("pwsh: %s", strings.Join(resp.Errors, "; "))
		}
		return resp, nil

	case <-ctx.Done():
		// 那一帧还堵在管道里,连接状态已经不干净。标记失效并断开,
		// 而不是留一个可能读到半帧的连接给下一次调用 —— 后者会给出「错但看起来对」的结果。
		// 读 goroutine 会因为管道关闭而返回,不会泄漏。
		//
		// 关闭放在后台:超时已经是调用方在等,再让它同步等 pwsh 退出(最多 closeBudget)
		// 等于把「等待」翻倍。dead 已经置位,后续调用会立刻拿到明确错误。
		c.dead.Store(true)
		go func() { _ = c.Close() }()
		return response{}, fmt.Errorf("pwsh: 等待响应超时: %w", ctx.Err())
	}
}

// Close 让 pwsh 自行退出;它不从就强杀。
//
// 幂等:call 的超时路径会先 Close,调用方的 cleanup 还会再 Close 一次。
func (c *Conn) Close() error {
	var err error
	c.once.Do(func() {
		c.dead.Store(true)

		// 关掉 stdin,session.ps1 读帧读到 EOF 就自己退出 —— 不留僵尸 pwsh。
		_ = c.stdin.Close()

		done := make(chan error, 1)
		go func() { done <- c.cmd.Wait() }()
		select {
		case err = <-done:
		case <-time.After(closeBudget):
			_ = c.cmd.Process.Kill()
			err = <-done
			err = fmt.Errorf("pwsh: 超时未退出,已强杀: %w", err)
		}
	})
	return err
}

// Pid 返回 pwsh 子进程的 pid(排错与 --status 用)。
func (c *Conn) Pid() int {
	if c.cmd.Process == nil {
		return 0
	}
	return c.cmd.Process.Pid
}

// Calls 返回这个会话已经执行过多少次请求。
func (c *Conn) Calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// Dead 报告这个会话是否已经失效。
//
// 失效是**单向**的:一旦帧失步(脚本往主机流写了东西)或读写超时,这个 Conn 就不能再用了
// —— 继续用它只会拿到"错但看起来对"的结果。调用方(② 层的 psEngine)据此决定**重建**
// 而不是接着用:换一个全新进程就能恢复,否则一次 Write-Host 会让该 daemon 的
// 所有 ② 层命令永久瘫痪。重建前仍应先 Close,把这个坏进程收干净。
func (c *Conn) Dead() bool { return c.dead.Load() }

func writeFrame(w io.Writer, body []byte) error {
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], uint32(len(body)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(body)
	return err
}

func readFrame(r *bufio.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.LittleEndian.Uint32(hdr[:])
	if n == 0 || n > maxFrameBytes {
		// 这正是「脚本往主机流写东西」会造成的症状。明确报错,
		// 不要试图把这段字节当 JSON 解析 —— 那只会给出更难查的错误。
		return nil, fmt.Errorf("响应帧长度不合理(%d 字节):帧已失步,通常是 ② 层脚本写了主机流", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// tailBuffer 只保留最后 limit 字节,给排错留线索但不吃内存。
type tailBuffer struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	limit int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf.Write(p)
	if t.buf.Len() > t.limit {
		kept := append([]byte(nil), t.buf.Bytes()[t.buf.Len()-t.limit:]...)
		t.buf.Reset()
		t.buf.Write(kept)
	}
	return len(p), nil
}

// hint 把收集到的 stderr 变成可操作错误信息的一部分。
func (t *tailBuffer) hint() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := strings.TrimSpace(t.buf.String())
	if s == "" {
		return ""
	}
	return "\n         pwsh stderr: " + strings.Join(strings.Split(s, "\n"), " | ")
}

// findPwsh 定位 pwsh.exe。
//
// PATH 优先;不在 PATH 时按候选路径找 —— 本机实测 pwsh 装在
// C:\Users\<用户>\Applications\PowerShell-7\(非标准位置),spec 第 1 节记过这件事:
// 「找不到解释器」是 Windows 上最典型的翻车形态,所以这里多找几处再放弃。
func findPwsh() string {
	if p, err := exec.LookPath("pwsh.exe"); err == nil {
		return p
	}
	candidates := []string{
		filepath.Join(os.Getenv("ProgramFiles"), "PowerShell", "7", "pwsh.exe"),
		filepath.Join(os.Getenv("ProgramFiles"), "PowerShell", "7-preview", "pwsh.exe"),
		filepath.Join(os.Getenv("LOCALAPPDATA"), "Microsoft", "WindowsApps", "pwsh.exe"),
		filepath.Join(os.Getenv("USERPROFILE"), "Applications", "PowerShell-7", "pwsh.exe"),
	}
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return "pwsh.exe"
}

// utf16LE 把脚本转成 -EncodedCommand 要的 UTF-16LE 字节。
func utf16LE(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := make([]byte, len(u)*2)
	for i, r := range u {
		binary.LittleEndian.PutUint16(b[i*2:], r)
	}
	return b
}
