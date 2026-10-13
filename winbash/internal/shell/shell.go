// Package shell 把 mvdan/sh 的 interp 包装成一个可复用的会话。
//
// 一个 Session 持有 interp.Runner 的全部状态(变量、cwd、函数),因此连续多次 Run
// 之间的 shell 状态是保持的 —— 这正是 AI 在 Windows 上本来缺失的东西。
package shell

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"

	"winbash/internal/builtins"
	"winbash/internal/errs"
	"winbash/internal/pwsh"
	"winbash/internal/winmgmt"
)

// Session 是一次命令行"会话":它跨多次 Run 保持变量、cwd 与函数。
//
// 不保证并发安全(interp.Runner 本身也不安全)。
type Session struct {
	runner *interp.Runner

	outBuf, errBuf bytes.Buffer
	stdout, stderr *switchWriter

	// jsonOut 是全局的 --json 开关,② 层命令据此决定输出 TSV 还是类型化 JSON。
	// 放在 Session 上而不是每次 Run 传下去:命令实现只拿到 ctx,
	// 而开关属于"这一次调用",由调用方在 Run 时设定。
	jsonOut bool

	// engine 是 ② 层的常驻 pwsh,第一次真的用到才启动(见 psEngine)。
	// engineErr 缓存启动失败,避免每次调用都重付一次启动超时。
	engine    *pwsh.Conn
	engineErr error

	// searchPath 是 ③ 层要用的搜索路径:真实 PATH + Git Bash 的 usr\bin。
	// 它在构造时算一次,存下来 —— 进程环境不动(Global Constraints),
	// 所以这条路径只活在 Session 里。
	searchPath string
}

// switchWriter 是一个输出目标可替换的 io.Writer。
//
// interp.StdIO 只能在构造 Runner 时设置一次,但每次 Run 需要把输出收进不同的 buffer,
// 所以让 writer 本身持有一个可替换的目标。加锁是因为 interp 在后台命令(`cmd &`)下
// 可能并发写。
type switchWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *switchWriter) Set(w io.Writer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.w = w
}

func (s *switchWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.w == nil {
		return len(p), nil
	}
	return s.w.Write(p)
}

// NewSession 建立一个会话,初始工作目录为当前进程的目录。
func NewSession() *Session {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}

	s := &Session{
		stdout:     &switchWriter{},
		stderr:     &switchWriter{},
		searchPath: BuildSearchPath(os.Getenv("PATH")),
	}

	runner, err := interp.New(
		interp.Dir(cwd),
		interp.Env(expand.ListEnviron(os.Environ()...)),
		interp.StdIO(nil, s.stdout, s.stderr),
		interp.ExecHandler(s.execHandler),
	)
	if err != nil {
		// interp.New 只在 RunnerOption 非法时报错;上面的选项都是常量,出错即编程错误。
		panic("shell: interp.New: " + err.Error())
	}
	s.runner = runner
	return s
}

// execHandler 是四层路由的入口。
//
// 计划里的四层是:① 纯 Go 内建 → ② Go 内建但底层转常驻 pwsh → ③ 真实 PATH → ④ 报错 + 建议。
// 之所以在 ExecHandler 这一层做路由,而不是改写 AST:路由要能看见「这条命令
// 是否真的跑起来了」,这个信息只有执行期才有。
func (s *Session) execHandler(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return nil
	}
	hc := interp.HandlerCtx(ctx)

	switch classify(args[0], s.searchPath) {
	case layerBuiltin:
		if code := builtins.Registry[args[0]](ctx, hc.Dir, args[1:], hc.Stdout, hc.Stderr); code != 0 {
			return interp.ExitStatus(code)
		}
		return nil

	case layerWinmgmt:
		conn, err := s.psEngine()
		if err != nil {
			fmt.Fprintln(hc.Stderr, errs.Errorf(args[0], err.Error(),
				"② 层靠 PowerShell 7 干活;先确认 pwsh 可用: winbash -c 'pwsh -NoProfile -c $PSVersionTable.PSVersion'"))
			return interp.ExitStatus(errs.ExitError)
		}
		if code := winmgmt.Registry[args[0]](ctx, conn, args[1:], s.jsonOut, hc.Stdout, hc.Stderr); code != 0 {
			return interp.ExitStatus(code)
		}
		return nil
	}

	// ③/④ 层:自己定位、自己执行。
	// **不用 interp.DefaultExecHandler**(R3 决定):它内部走 interp.LookPath,
	// 读的是**进程 PATH**,不受我们构造的 searchPath 控制 —— 两者接不上,
	// 而 Global Constraints 不允许改环境变量。
	return s.runExternal(ctx, args, hc)
}

// runExternal 是 ③ 层(命中真实 PATH)与 ④ 层(哪个都没有)的实现。
func (s *Session) runExternal(ctx context.Context, args []string, hc interp.HandlerContext) error {
	abs, ok := Which(args[0], s.searchPath)
	if !ok {
		// ④ 层。这里只给最基础的可操作错误;已知坑的翻译表(Task 13)接在同一处。
		fmt.Fprintln(hc.Stderr, errs.Errorf(args[0], "找不到这个命令",
			"确认命令名有没有写错;③ 层会搜索真实 PATH 与 Git Bash 的 usr\\bin,可用 winbash -c 'which "+args[0]+"' 自查"))
		return interp.ExitStatus(errs.ExitNotFound)
	}

	cmd := exec.CommandContext(ctx, abs, args[1:]...)
	cmd.Dir = hc.Dir
	cmd.Stdin = hc.Stdin
	cmd.Stdout = hc.Stdout
	cmd.Stderr = hc.Stderr
	cmd.Env = envList(hc.Env)

	err := cmd.Run()
	if err == nil {
		return nil
	}

	// 子进程自己的非零退出码原样带回去 —— 这是 bash 的语义(`grep 没匹配 → 1`),
	// 也是 ③ 层与 ④ 层的分界:跑起来了就不该替它改写结果。
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return interp.ExitStatus(ee.ExitCode())
	}

	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(hc.Stderr, errs.Errorf(args[0], err.Error(), "文件已经不在了(它可能在两条命令之间被删掉或被换掉)"))
		return interp.ExitStatus(errs.ExitNotFound)
	}

	fmt.Fprintln(hc.Stderr, errs.Errorf(args[0], err.Error(), "确认它是不是可执行文件,以及当前用户有没有权限运行它"))
	return interp.ExitStatus(errs.ExitNoExec)
}

// envList 把 interp 的环境展开成 os/exec 要的形式。
//
// 用 hc.Env 而不是 os.Environ():shell 里的 `export FOO=bar` 必须对 ③ 层的子进程可见 ——
// 否则「在 winbash 里导出变量再跑 git」会是**静默无效**的,而那正是本项目要消灭的失败形态。
// 只带导出过的变量,与 POSIX 的语义一致(局部变量不给子进程)。
func envList(env expand.Environ) []string {
	var out []string
	env.Each(func(name string, vr expand.Variable) bool {
		if name == "" || !vr.Exported || !vr.IsSet() {
			return true
		}
		out = append(out, name+"="+vr.String())
		return true
	})
	return out
}

// psEngine 返回 ② 层的常驻 pwsh,第一次真的用到时才启动。
//
// 惰性是有意的:只跑 ① 层的会话(例如 `ls | grep x`)不该付那次 ~350ms 的启动成本。
// 失败结果同样缓存 —— 否则 `svc list; svc status x` 会为同一个错误各等一次超时。
func (s *Session) psEngine() (*pwsh.Conn, error) {
	if s.engine != nil {
		return s.engine, nil
	}
	if s.engineErr != nil {
		return nil, s.engineErr
	}
	conn, err := pwsh.Start()
	if err != nil {
		s.engineErr = err
		return nil, err
	}
	s.engine = conn
	return conn, nil
}

// Close 释放会话持有的资源(目前只有 ② 层的 pwsh 引擎)。
//
// daemon 退出时必须调用:否则每重启一次 daemon 就留下一个常驻 pwsh 进程。
func (s *Session) Close() error {
	if s.engine == nil {
		return nil
	}
	err := s.engine.Close()
	s.engine = nil
	return err
}

// Dir 返回会话当前的工作目录。cd 之后它会跟着变。
func (s *Session) Dir() string { return s.runner.Dir }

// Run 执行一段脚本,返回 stdout、退出码和错误行。
//
// 退出码沿用 shell 惯例:0 成功,非零失败;语法解析失败返回 2。
// jsonOut 是本次调用的 --json 开关,② 层命令据此改用类型化 JSON 输出。
func (s *Session) Run(ctx context.Context, script string, jsonOut bool) (stdout string, exit int, errs []string) {
	s.jsonOut = jsonOut

	s.outBuf.Reset()
	s.errBuf.Reset()
	s.stdout.Set(&s.outBuf)
	s.stderr.Set(&s.errBuf)
	defer func() {
		s.stdout.Set(nil)
		s.stderr.Set(nil)
	}()

	prog, perr := syntax.NewParser().Parse(strings.NewReader(script), "")
	if perr != nil {
		return "", 2, []string{perr.Error()}
	}

	runErr := s.runner.Run(ctx, prog)

	if runErr != nil {
		var status interp.ExitStatus
		if errors.As(runErr, &status) {
			exit = int(status)
		} else {
			exit = 1
		}
	}

	errs = nonEmptyLines(s.errBuf.String())
	if runErr != nil && exit != 0 {
		errs = append(errs, runErr.Error())
	}
	return s.outBuf.String(), exit, errs
}

// nonEmptyLines 把 stderr 拆成非空行,用于 Response.Errors。
func nonEmptyLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}
