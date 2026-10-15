// Package client 是 winbash 的轻客户端:连上常驻 daemon,发一条脚本,取回结果。
package client

import (
	"context"
	"errors"
	"fmt"
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

// RunTimeout 同 Run,但给整个往返设一个上限(timeout<=0 表示不设限)。
//
// 这个上限会同时传给 daemon(见 ipc.Request.TimeoutMS):client 放弃不代表
// daemon 该继续跑,否则一条无限循环会把后续命令全部饿死。
//
// idle 只在**这次调用把 daemon 拉起来**时用得上(见 spawnDaemon)。timeout 与 idle
// 是两件事:前者是"这一条命令跑多久算超时",后者是"daemon 空着多久就自己走"。
//
// 嵌套里(见 inSession)不走 daemon,直接在本进程执行 —— 理由见 runLocal。
func RunTimeout(endpoint, script, cwd string, jsonOut bool, timeout, idle time.Duration) (ipc.Response, error) {
	if inSession() {
		return runLocal(script, jsonOut, timeout)
	}
	return oneShot(endpoint, ipc.Request{Script: script, Cwd: cwd, JSON: jsonOut}, false, timeout, idle)
}

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
	resp, err := oneShot(endpoint, ipc.Request{Op: "stop"}, true, 0, 0)
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("client: daemon 拒绝停止: %s", strings.Join(resp.Errors, "; "))
	}
	return nil
}

// Status 取 daemon 的状态(pid / cwd / ② 层引擎)。同样不拉起 daemon。
func Status(endpoint string) (string, error) {
	if inSession() {
		// 嵌套里看不了 daemon 状态:去连它只会等到 300s 超时(理由见 EnvNested)。
		return "", fmt.Errorf("client: 你正跑在一个 winbash 会话里,看不到 daemon 状态" +
			"(嵌套的 winbash 不连 daemon);要看状态请在外面执行 winbash --status")
	}
	resp, err := oneShot(endpoint, ipc.Request{Op: "status"}, true, 0, 0)
	if err != nil {
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
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow,
	}
	return cmd.Start()
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
// 为什么嵌套必须走这条路:daemon 一条连接串行处理(daemon.Serve),而这次调用正是
// 从某条**正在 daemon 里跑着的**命令派生出来的。去连它 = 外层等内层、内层等外层,
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
