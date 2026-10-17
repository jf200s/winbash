// Package client 是 winbash 的轻客户端:连上常驻 daemon,发一条脚本,取回结果。
package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"winbash/internal/ipc"
	"winbash/internal/shell"
)

const (
	dialTimeout    = 3 * time.Second
	startupBudget  = 20 * time.Second
	pollInterval   = 20 * time.Millisecond
	createNoWindow = 0x08000000 // CREATE_NO_WINDOW
)

// endpointInfo 是端口文件的内容:daemon 地址 + 鉴权 token。
//
// token 为什么要跟地址放一起:只发地址正是修之前那个洞 —— 同机任何能读这个文件的
// 进程(实测 ACL 里带着沙箱组)都能借 daemon 的身份执行命令。
type endpointInfo struct {
	addr  string
	token string
}

// Run 连上 endpoint 指向的 daemon 执行 script。
//
// daemon 没起来时先拉起它;若 daemon 刚死而端口文件还在,清掉陈旧文件后重拉一次
// (Review Focus 第 5 条:daemon 死后 client 必须自行恢复,不能挂起等待)。
//
// idle=0 表示"不指定",由 daemon 侧取自己的默认值 —— 这是给测试与库调用方的入口;
// 命令行一律走 RunTimeout 并把 --idle 显式传下去。
func Run(endpoint, script, cwd string, jsonOut bool) (ipc.Response, error) {
	return RunTimeout(endpoint, script, cwd, jsonOut, 0, 0)
}

// Option 是 RunTimeout 的可选开关。
//
// 用可变参数而不是再加形参:现有调用点(嵌套、测试、库调用方)一行都不用改。
type Option func(*ipc.Request)

// Fresh 让这条命令跑在**一次性会话**里:之前的命令影响不到它,它设的变量 /
// set -e / trap 也不留给后面的命令。语义与代价见 ipc.Request.Fresh。
func Fresh() Option { return func(r *ipc.Request) { r.Fresh = true } }

// Session 让这条命令落到指定的会话里。空串等同于「不指定」(默认会话)。
//
// 同名会话内仍然串行且状态持久;不同名会话之间才并行。语义见 ipc.Request.Session。
func Session(name string) Option { return func(r *ipc.Request) { r.Session = name } }

// RunTimeout 同 Run,但给整个往返设一个上限(timeout<=0 表示不设限)。
//
// 这个上限会同时传给 daemon(见 ipc.Request.TimeoutMS):client 放弃不代表
// daemon 该继续跑,否则一条无限循环会把后续命令全部饿死。
//
// idle 只在**这次调用把 daemon 拉起来**时用得上(见 spawnDaemon)。timeout 与 idle
// 是两件事:前者是"这一条命令跑多久算超时",后者是"daemon 空着多久就自己走"。
//
// 嵌套里(见 inSession)不走 daemon,直接在本进程执行 —— 理由见 runLocal。
func RunTimeout(endpoint, script, cwd string, jsonOut bool, timeout, idle time.Duration, opts ...Option) (ipc.Response, error) {
	if inSession() {
		// 嵌套时 runLocal 本来就每次新建 Session,天然是 fresh 的,所以 opts 在这里
		// 无事可做 —— 写出来是为了别让人以为漏了。
		return runLocal(script, jsonOut, timeout)
	}
	req := ipc.Request{
		Proto:     ipc.ProtoVersion,
		Script:    script,
		Cwd:       cwd,
		JSON:      jsonOut,
		StdinMode: ipc.StdinNone,
	}
	for _, o := range opts {
		o(&req)
	}
	if stdinIsStream() {
		req.StdinMode = ipc.StdinStream
		req.StdinLen = ipc.StdinUntilEOF // 管道输入没有确定长度,读到半侧关闭为止
	}
	resp, err := oneShot(endpoint, req, false, timeout, idle)
	if err != nil {
		return resp, err
	}
	// 只在 run 这条路上查版本,与 daemon 侧的检查对称。--stop 走的是 Stop,
	// 刻意不查 —— 否则「版本不匹配时先 --stop」这个推荐处置自己会被挡掉。
	if perr := ipc.CheckProto("daemon", resp.Proto); perr != nil {
		return ipc.Response{}, fmt.Errorf("client: %s", perr.Error())
	}
	return resp, nil
}

// stdinIsStream 判断调用方的 stdin 该不该走流式转发。
//
// **只在 stdin 不是字符设备时转发** —— 理由:无法从脚本文本判断它会不会读 stdin
// (脚本是任意的),交互终端下无条件读会让 `winbash -c 'ls'` 这类**不读** stdin 的
// 命令永远挂住。代价:终端下 `winbash -c 'cat'` 拿到空输入,而真 bash 会读 tty ——
// 这条差异记在 README 的已知差异表里,不是静默。
//
// 流式相对一次性(2026-10-16 之前的实现)的变化:不再 `io.ReadAll` 整个 stdin,
// 所以**没有字节上限**、也不再要求上游先给 EOF —— `yes | winbash -c 'head -1'`
// 这类场景由边读边转 + daemon 侧的 stop 时序解决,而不是靠缓存。
func stdinIsStream() bool {
	info, err := os.Stdin.Stat()
	if err != nil {
		return false // 查不到就当没有 stdin,不拦路
	}
	return info.Mode()&os.ModeCharDevice == 0
}

// manageTimeout 是 --status / --stop 这类**管理操作**的上限。
//
// 它们原先传 timeout=0(不设上限),而 exchange 里只有 timeout>0 才 SetDeadline ——
// 于是 daemon 一时没回话,`winbash --status` 就会**永久挂起**。当年的实测(daemon
// 串行、忙时):`timeout 5 winbash --status` 退出码 124(被外层杀掉),而同样忙的
// 状态下 `--timeout 2s -c 'echo x'` 是准时返回的。
//
// 现在 daemon 并发处理连接、op 分派在会话队列**之前**(daemon.handleConn),所以
// 「被一条长命令占着」已经不是管理操作会遇到的形态 —— 这个上限只剩**最后的保险**:
// daemon 卡在关停、或已经不再接受连接时,让调用方拿到一句说明而不是无限等。
//
// 管理操作的意义恰恰是「看它活着没」,无限等是最坏的失败形态:看上去像挂死,
// 使用者只能 Ctrl+C —— 那比一句「它可能卡住了」糟得多。
const manageTimeout = 5 * time.Second

// Stop 让 daemon 退出。
//
// mustExist 的语义在这里很要紧:daemon 没在跑时**不把它拉起来** ——
// 「停一个没在跑的 daemon」不该先把 daemon 启动、再把它停掉。
func Stop(endpoint string) error {
	if inSession() {
		// 要停的正是**自己所在的**那个 daemon —— 停掉它,跑着这次调用的会话也就没了。
		return fmt.Errorf("client: 你正跑在一个 winbash 会话里,不能停掉自己所在的 daemon" +
			"(嵌套的 winbash 是独立的,它不连 daemon);要停请在外面执行 winbash --stop")
	}
	resp, err := oneShot(endpoint, ipc.Request{Op: "stop"}, true, manageTimeout, 0)
	if err != nil {
		if isTimeout(err) {
			return fmt.Errorf("client: --stop 在 %v 内没有响应 —— daemon 可能卡在关停上"+
				"(它已经不再接受新命令了,不是在排队);稍后重试一次,或看 --status", manageTimeout)
		}
		return err
	}
	if !resp.OK {
		return fmt.Errorf("client: daemon 拒绝停止: %s", strings.Join(resp.Errors, "; "))
	}
	return nil
}

// Status 取 daemon 的状态(pid / cwd / ② 层引擎 / 会话池明细)。同样不拉起 daemon。
func Status(endpoint string) (string, error) {
	if inSession() {
		// 嵌套里看不了 daemon 状态:去连它只会等到 300s 超时(理由见 EnvNested)。
		return "", fmt.Errorf("client: 你正跑在一个 winbash 会话里,看不到 daemon 状态" +
			"(嵌套的 winbash 不连 daemon);要看状态请在外面执行 winbash --status")
	}
	resp, err := oneShot(endpoint, ipc.Request{Op: "status"}, true, manageTimeout, 0)
	if err != nil {
		if isTimeout(err) {
			return "", fmt.Errorf("client: --status 在 %v 内没有响应 —— daemon 可能卡在关停上"+
				"(它已经不再接受新命令了,不是在排队);稍后重试一次", manageTimeout)
		}
		return "", err
	}
	return string(resp.Out), nil
}

// oneShot 发一个请求收一个响应。
//
// mustExist=true:daemon 不在就报错(给 --stop/--status 用);
// false:daemon 不在就把它拉起来(给执行脚本用)。
func oneShot(endpoint string, req ipc.Request, mustExist bool, timeout, idle time.Duration) (ipc.Response, error) {
	info, err := dialAddr(endpoint, mustExist, idle)
	if err != nil {
		return ipc.Response{}, err
	}
	req.Token = info.token
	if timeout > 0 {
		// daemon 侧也要知道上限:client 放弃不代表它该继续跑(见 ipc.Request.TimeoutMS)。
		req.TimeoutMS = int(timeout / time.Millisecond)
	}

	conn, err := dial(endpoint, info.addr, mustExist, idle)
	if err != nil {
		return ipc.Response{}, err
	}

	resp, xerr := exchange(conn, req, timeout)
	conn.Close()
	if xerr == nil {
		if resp.Auth {
			return retryAfterAuthFailure(endpoint, req, timeout, idle)
		}
		return resp, nil
	}

	// ★ 超时**绝不重试**。
	//
	// 这里原本对任何失败都"清端口文件、重拉 daemon、原样重跑",理由是"请求多半没送达"。
	// 实测推翻了这个理由:`echo RUN >> f; sleep 6` 配 --timeout 2s,文件里出现**两行** ——
	// 请求送达了、跑起来了、只是 client 先放弃了。对 mkdir / git commit / 写文件
	// 这类脚本,重放就是数据损坏。
	if isTimeout(xerr) {
		return ipc.Response{}, xerr
	}
	// --stop / --status 不该把 daemon 拉起来。
	if mustExist {
		return ipc.Response{}, xerr
	}

	// 剩下这类才是"半路上掉了":连接被重置、daemon 刚被杀。这种请求**极可能没送达**,
	// 重试一次是「从偶发竞态里恢复」和「不在真故障上打转」之间的中点。
	forgetStaleEndpoint(endpoint, info.addr)
	info2, aerr := ensureDaemon(endpoint, idle)
	if aerr != nil {
		return ipc.Response{}, xerr
	}
	conn2, derr := dial(endpoint, info2.addr, false, idle)
	if derr != nil {
		return ipc.Response{}, xerr
	}
	defer conn2.Close()
	req.Token = info2.token
	return exchange(conn2, req, timeout)
}

// retryAfterAuthFailure 在鉴权失败后重读端口文件再试一次。
//
// 为什么这次重试是安全的:鉴权失败意味着 daemon **根本没有执行**请求,
// 所以不存在"脚本跑两遍"的风险 —— 与上面超时分支形成对照,那里重试才是危险的。
//
// 最常见的成因不是攻击,而是竞态:我们读完地址与 token 之后,那个 daemon 恰好退出、
// 新 daemon 把同一个端口文件改写成了自己的地址与 token。重读一次就拿到新的了。
//
// 重试仍失败就把原来的 401 原样交上去 —— 那才可能是真的有人在乱连。
func retryAfterAuthFailure(endpoint string, req ipc.Request, timeout, idle time.Duration) (ipc.Response, error) {
	info2, err := readEndpoint(endpoint)
	if err != nil {
		return ipc.Response{}, fmt.Errorf("client: daemon 鉴权失败,且端口文件已不可读: %w", err)
	}
	conn2, err := dial(endpoint, info2.addr, false, idle)
	if err != nil {
		return ipc.Response{}, err
	}
	defer conn2.Close()
	req.Token = info2.token
	resp, xerr := exchange(conn2, req, timeout)
	if xerr != nil {
		return ipc.Response{}, xerr
	}
	return resp, nil
}

// isTimeout 判断错误链里有没有一个超时(连接 deadline 到了)。
//
// 必须走 errors.As 而不是字符串比较:exchange 用 %w 包了一层,
// 而真正带 Timeout() 的是底下的 *net.OpError。
func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// dial 建立到 addr 的连接;mustExist 时连不上就直接报"没在运行"。
//
// idle 只在"端口文件指向的 daemon 已经不在了、得重新拉一个"时才用得上。
func dial(endpoint, addr string, mustExist bool, idle time.Duration) (net.Conn, error) {
	conn, err := net.DialTimeout("tcp", addr, dialTimeout)
	if err == nil {
		return conn, nil
	}
	if mustExist {
		return nil, fmt.Errorf("client: daemon 没在运行(端口文件 %s 指向的地址连不上)", endpoint)
	}
	// 端口文件指向的 daemon 已经不在了。清掉陈旧端口再拉一次。
	forgetStaleEndpoint(endpoint, addr)
	info, aerr := ensureDaemon(endpoint, idle)
	if aerr != nil {
		return nil, aerr
	}
	conn, err = net.DialTimeout("tcp", info.addr, dialTimeout)
	if err != nil {
		return nil, fmt.Errorf("client: 连接 daemon 失败: %w", err)
	}
	return conn, nil
}

// forgetStaleEndpoint 只在端口文件**仍然指向 addr** 时才删它。
//
// 与 daemon 侧的 removeIfOwned 是同一个理由:这个窗口里可能有新 daemon 已经把
// 文件改写成自己的地址了,照删不误会把新 daemon 变成"谁也找不到"的孤儿。
func forgetStaleEndpoint(endpoint, addr string) {
	b, err := os.ReadFile(endpoint)
	if err != nil {
		return
	}
	first := strings.TrimSpace(strings.SplitN(strings.TrimSpace(string(b)), "\n", 2)[0])
	if first == addr {
		_ = os.Remove(endpoint)
	}
}

// exchange 在一条连接上完成一次"写请求 + 读响应"。
func exchange(conn net.Conn, req ipc.Request, timeout time.Duration) (ipc.Response, error) {
	if timeout > 0 {
		// 给整个往返设上限:daemon 卡在一条长命令上时,使用者要的是"明确超时",
		// 不是"无限等下去"(后者看起来像挂死)。
		_ = conn.SetDeadline(time.Now().Add(timeout))
	}

	body, err := ipc.Encode(req)
	if err != nil {
		return ipc.Response{}, err
	}
	if err := ipc.WriteMessage(conn, body); err != nil {
		return ipc.Response{}, fmt.Errorf("client: 发送请求失败: %w", err)
	}

	// 流式 stdin:请求头之后把 os.Stdin 的字节接着灌给 daemon,灌完关**写半侧** ——
	// daemon 靠那个 FIN 认出「stdin 到此为止」(协议里 StdinLen=-1 的语义)。
	//
	// 起 goroutine 是必须的:上游不给 EOF 时 io.Copy 会一直读,而主流程必须马上去
	// ReadMessage 等响应。读与写发生在同一连接的**两个方向**,TCP 全双工下安全。
	//
	// 这支不 join:client 是短命进程,主流程拿到响应就返回、进程随之退出,
	// 上游永不 EOF 把它卡住也不阻塞主流程。真正必须精确回收的是 daemon 侧那支
	// (见 daemon.startStdinStream 的 stop)—— 它若不回收,那个会话的队列会被一直占着,
	// 投给同一个会话的后续命令全部排不上。
	if req.StdinMode == ipc.StdinStream {
		go func() {
			_, _ = io.Copy(conn, os.Stdin)
			// 写完关写半侧:仍能读响应。Windows 上由 ws2_32.shutdown 实现,
			// 项目 daemon.Serve 收尾处已在用同一调用。
			if tc, ok := conn.(*net.TCPConn); ok {
				_ = tc.CloseWrite()
			}
		}()
	}

	raw, err := ipc.ReadMessage(conn)
	if err != nil {
		return ipc.Response{}, fmt.Errorf("client: 读取响应失败: %w", err)
	}
	return ipc.DecodeResponse(raw)
}

// dialAddr 决定往哪个地址连。mustExist 时只读端口文件,绝不拉起新 daemon。
func dialAddr(endpoint string, mustExist bool, idle time.Duration) (endpointInfo, error) {
	if !mustExist {
		return ensureDaemon(endpoint, idle)
	}
	info, err := readEndpoint(endpoint)
	if err != nil {
		return endpointInfo{}, fmt.Errorf("client: daemon 没在运行(端口文件 %s 不存在)", endpoint)
	}
	return info, nil
}

// ensureDaemon 返回端口文件内容;daemon 不在就先把它拉起来。
//
// idle 会原样转给新起的 daemon。**这件事必须做到**:不转发的话,使用者写的
// `--idle 3s` 会被静默丢弃,自动拉起的 daemon 永远用默认的 30 分钟 ——
// 属于"文档承诺了、实现没转发",而这类落差最伤信任(实测:不转发时等 6s 后
// daemon 仍在、端口文件仍在)。
func ensureDaemon(endpoint string, idle time.Duration) (endpointInfo, error) {
	if info, err := readEndpoint(endpoint); err == nil {
		return info, nil
	}

	// 从这里往下是「没有 daemon,要拉起一个」——必须跨进程互斥。
	//
	// 不互斥会怎样(实测):8 个 client 同时首次调用,每个都读到「没有 daemon」,
	// 于是各 spawn 一个。daemon 用 127.0.0.1:0 取随机端口,所以**它们全都绑定成功**,
	// 不存在"后来者失败退出"这种天然去重 —— 结果 8 个 --serve 进程同时活着,
	// 端口文件只指向最后写入的那一个,其余 7 个是孤儿:--status 看不到、--stop 停不掉,
	// 各自还抱着一个 Session(用到 ② 层的还各抱一个 pwsh)白占内存,要等 30 分钟空闲才退。
	release, alreadyUp, err := acquireStartLock(endpoint)
	if err != nil {
		return endpointInfo{}, err
	}
	if alreadyUp {
		// 等锁的那段时间里,持锁者已经把 daemon 起来了。
		return readEndpoint(endpoint)
	}
	defer release()

	// 抢到锁之后要**再读一次**:上一个持锁者可能刚好在放锁之前写好了端口文件。
	// 少了这一次,我们就会在一个已经健康的 daemon 旁边再起一个 —— 正是要消灭的那件事。
	if info, err := readEndpoint(endpoint); err == nil {
		return info, nil
	}
	if err := spawnDaemon(endpoint, idle); err != nil {
		return endpointInfo{}, fmt.Errorf("client: 启动 daemon 失败: %w", err)
	}
	deadline := time.Now().Add(startupBudget)
	for time.Now().Before(deadline) {
		if info, err := readEndpoint(endpoint); err == nil {
			return info, nil
		}
		time.Sleep(pollInterval)
	}
	return endpointInfo{}, fmt.Errorf("client: daemon 在 %s 内没有起来", startupBudget)
}

// startLockPath 是与端点配套的启动锁文件路径。
//
// 放在端口文件旁边(同一个目录),所以端点隔离到哪、锁就隔离到哪 —— 两个跑在不同
// --endpoint 上的测试不会互相挡。
func startLockPath(endpoint string) string { return endpoint + ".lock" }

// staleLockBudget 是判定「这把锁是陈旧残留」的时长。
//
// 必须**明显大于** startupBudget(20s):持锁者正常工作时最长就是花这么久等 daemon 起来,
// 这个时长之内都不能怀疑它。留 10s 余量。
const staleLockBudget = startupBudget + 10*time.Second

// acquireStartLock 抢「拉起 daemon」这把跨进程锁。
//
// 三个返回值各有用途:
//   - release != nil:抢到了,调用方必须在 spawn + 等就绪之后调用它(持锁只覆盖这段,
//     **绝不覆盖命令执行** —— 否则并发就没了)。
//   - alreadyUp:没抢到锁,但等锁期间端口文件出现了,说明别人已经起好了,直接用。
//   - err:真失败(建不了锁文件,或等到 startupBudget 还是没结果)。
//
// 为什么用「锁文件 + O_EXCL」而不是命名互斥量:后者要直接引 golang.org/x/sys/windows,
// 而本项目的硬约束是「唯一直接依赖 mvdan.cc/sh/v3」(x/sys 目前只是它带进来的间接依赖)。
// 为一把锁破这条约束不划算;O_CREATE|O_EXCL 在 Windows 上同样是原子的。
func acquireStartLock(endpoint string) (release func(), alreadyUp bool, err error) {
	path := startLockPath(endpoint)
	deadline := time.Now().Add(startupBudget)
	for {
		f, oerr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if oerr == nil {
			// 写 pid 与时间戳只为事后排查"这把锁是谁留下的",判定陈旧看的是 mtime。
			_, _ = fmt.Fprintf(f, "%d\n%d\n", os.Getpid(), time.Now().UnixNano())
			_ = f.Close()
			return func() { _ = os.Remove(path) }, false, nil
		}
		if !os.IsExist(oerr) {
			return nil, false, fmt.Errorf("client: 建启动锁 %s 失败: %w", path, oerr)
		}

		// 别人正在拉起 daemon。它一起来端口文件就会出现,所以每一轮都先看这个 ——
		// 比单纯"等锁消失"更快,也不怕持锁者忘了删锁。
		if _, rerr := readEndpoint(endpoint); rerr == nil {
			return nil, true, nil
		}

		// 持锁者可能被强杀(taskkill、宿主回收),锁文件会留下。
		// 不靠"读 pid 再判断进程存活":Windows 上 os.FindProcess 恒成功,
		// 真要做存活探测就得引平台代码 —— 与上面避开 x/sys 是同一条理由。
		if st, serr := os.Stat(path); serr == nil && time.Since(st.ModTime()) > staleLockBudget {
			if rmerr := os.Remove(path); rmerr == nil {
				continue // 接管成功,立刻重抢
			}
			// 删不掉(权限/被占用)就别空转,落到下面的超时分支等它自己过期。
		}

		if time.Now().After(deadline) {
			return nil, false, fmt.Errorf(
				"client: 等其它 winbash 进程拉起 daemon 超时(%s);锁文件 %s 仍在。"+
					"若确实没有别的 winbash 在跑,删掉这个锁文件再试", startupBudget, path)
		}
		time.Sleep(pollInterval)
	}
}

// readEndpoint 解析端口文件的两行:地址 + token。
//
// token 允许缺失(老 daemon 或外部工具写出来的文件),那时它会被当成空串,
// daemon 侧会拒绝 —— 这是有意的:缺少凭据应该是"被拒",不是"放行"。
func readEndpoint(endpoint string) (endpointInfo, error) {
	b, err := os.ReadFile(endpoint)
	if err != nil {
		return endpointInfo{}, err
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	info := endpointInfo{addr: strings.TrimSpace(lines[0])}
	if info.addr == "" {
		return endpointInfo{}, fmt.Errorf("client: 端口文件 %s 是空的", endpoint)
	}
	if len(lines) > 1 {
		info.token = strings.TrimSpace(lines[1])
	}
	return info, nil
}

// spawnDaemon 以 --serve 重新启动自己。隐藏窗口,不打扰使用者。
func spawnDaemon(endpoint string, idle time.Duration) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, serveArgs(endpoint, idle)...)
	// 子进程不能继承这几个环境变量:它们是**client 侧**的默认值来源,而 daemon
	// 一旦拿到就会撞上 main.go 里那些「只用于执行脚本」的互斥校验。
	// 实测后果:设了 WINBASH_SESSION_NAME 之后 `--serve` 直接 exit 2,daemon 永远
	// 起不来,连 `--stop` 这个唯一处置也一起废 —— 而那个变量正是 README 推荐
	// 并行子代理去设的。
	cmd.Env = daemonEnv(os.Environ())
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow,
	}
	return cmd.Start()
}

// daemonEnv 从环境里摘掉那些「只该影响 client」的变量,给拉起的 daemon 用。
//
// 理由:这些变量的作用都是给 **CLI 的 flag 默认值**提供来源(见 cmd/winbash/main.go),
// 而 daemon 是用 `--serve` 起的 —— 它一旦拿到默认值就会撞上「--session/--fresh 只用于
// 执行脚本」那几条互斥校验,于是直接 exit 2。
//
// 注意**不摘** EnvNested:那一条由 shell.NewSession 在 daemon 内部自己滤,与这里无关。
func daemonEnv(env []string) []string {
	drop := map[string]bool{
		"WINBASH_SESSION_NAME": true,
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		// Windows 的环境变量名大小写不敏感,比对时必须折成大写。
		if drop[strings.ToUpper(name)] {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// serveArgs 拼出拉起的 daemon 的命令行。抽成纯函数是为了能测 ——
// spawnDaemon 自己必须构建真 exe 才跑得起来(测试进程里 os.Executable() 是
// winbash.test,它不认识 --serve),但"参数有没有拼对"是这里最容易出错、
// 也最值得钉的一环。
//
// idle>0 时把 `--idle <时长>` 拼进去。**这一条不能省**:不拼的话使用者显式写的
// `--idle 3s` 会被静默丢掉,自动拉起的 daemon 永远用默认的 30 分钟 ——
// 实测过:不转发时 `winbash --idle 3s -c 'echo x'` 等 7s 后 daemon 仍在、
// 端口文件仍在;转发后同一命令 7s 内自己走干净。
//
// idle<=0 时**不传**这个开关:`--idle 0` 是另一个意思("永不退出"),
// 传过去会改变 daemon 的默认行为,而不是表达"没指定"。
func serveArgs(endpoint string, idle time.Duration) []string {
	args := []string{"--serve", "--endpoint", endpoint}
	if idle > 0 {
		args = append(args, "--idle", idle.String())
	}
	return args
}

// inSession 报告「这次调用是不是从 winbash 会话里发起的」。
//
// 判据就是 EnvNested:它由 ②/③ 层派生给子进程,所以只在「winbash 里的命令又调
// winbash」时为真。daemon 自己不会带着它 —— NewSession 会把继承来的滤掉,否则
// 从嵌套里拉起的那个 daemon 会让之后**所有**调用都误判成嵌套。
func inSession() bool {
	return os.Getenv(shell.EnvNested) != ""
}

// runLocal 在**本进程内**跑一段脚本,不连 daemon。
//
// 为什么嵌套必须走这条路:这次调用正是从**某条正在 daemon 里跑着的**命令派生出来的,
// 而会话内的请求是严格 FIFO 串行的(每会话一个 worker)。内层去连同一个 daemon、又落到
// 同一个会话,就得排在父命令后面;父命令却在等内层退出。去连它 = 外层等内层、内层等外层,
// 两边僵到 client 的 300s 超时才散场。真 bash 的嵌套 bash 是两个独立进程,
// 这里照样:起一个一次性会话,跑完就收。
//
// 代价是这一次丢掉常驻带来的速度(新建 ① 层状态;用到 ② 层时引擎再起,~350ms)。
// 嵌套本来就不是热路径,而且这里的取舍很明确:**正确**优先于快。
//
// 关于 cwd:唯一调用方是 cmd/winbash,它传的就是本进程的 cwd,而 NewSession 本来
// 就以进程 cwd 起始,所以这里不需要再切一次。
func runLocal(script string, jsonOut bool, timeout time.Duration) (ipc.Response, error) {
	sess := shell.NewSession()
	defer sess.Close()

	// stdin 在嵌套里同样要接上。漏了它的话 `echo x | winbash -c "… | winbash -c cat'"`
	// 会是「外层通、内层空」—— 半能用比完全不能用更费人。
	//
	// 这里读的是内层进程自己的 os.Stdin,而它正是外层 ③/② 层派生子进程时接上的
	// 那个 reader(shell.go 的 cmd.Stdin = hc.Stdin),所以与真 bash 的嵌套一致:
	// 同一个源,顺序消费,外层已读掉的部分内层拿不到。
	//
	// 用 StdinPipe + io.Copy 而不是「先读进内存再 SetStdinBytes」:那样会让嵌套
	// 路径也要求上游先给 EOF(上游不给就挂住),与主路径同样的毛病分叉两处。
	// 这支 goroutine 不 join —— 嵌套是一次性会话,Run 返回后 closeStdin 关掉读端,
	// 写端下次 Write 就失败(Windows 上是 Errno 232),它自己退出;进程也要退了。
	if stdinIsStream() {
		if pw, perr := sess.StdinPipe(); perr == nil {
			go func() {
				_, _ = io.Copy(pw, os.Stdin)
				_ = pw.Close()
			}()
		}
		// 接不通管道时静默退回空输入,与 SetStdinBytes 的退化行为一致:
		// 建管道失败是系统级故障,让命令立刻 EOF 好过把嵌套调用挂死。
	}

	// timeout 在嵌套里也生效。原先这里写死 context.Background(),于是
	// `winbash --timeout 1s -c 'sleep 5'` 在嵌套下会真的睡满 5 秒 ——
	// README 说 --timeout 是"单次请求超时",嵌套里却静默失效。
	ctx := context.Background()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	out, exit, errs := sess.Run(ctx, script, jsonOut)
	if cerr := ctx.Err(); cerr != nil {
		// os.ErrDeadlineExceeded 带 Timeout() 方法,与 daemon 路径的超时是同一类,
		// 上游能一眼认出"这是超时,不是脚本本身失败"。
		return ipc.Response{}, fmt.Errorf("嵌套执行超过 %s: %w", timeout, os.ErrDeadlineExceeded)
	}
	return ipc.Response{OK: exit == 0, Out: []byte(out), Errors: errs, Exit: exit}, nil
}
