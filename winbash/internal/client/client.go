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
	"strconv"
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

// Args 给这条命令带上位置参数:args[0] 绑成脚本的 $0,args[1:] 依次绑成 $1/$2/…。
// 与 bash 的两种绑法一致 —— `bash -c 'script' name arg1` 与脚本文件 `./s.sh a b`
// 都是第一个参数当 $0、其余当 $1+。不传 = 无位置参数,与引入本开关之前完全等价。
//
// 为什么要按参数传而不是把位置参数拼进脚本文本:拼接会把参数与代码混在一起
// 重新解释,引号、空格、`$` 都会变义 —— `winbash -c 'echo $1' 'a b'` 里的空格
// 必须原样活到 $1,拼接做不到。
func Args(args ...string) Option { return func(r *ipc.Request) { r.Args = args } }

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
	req := ipc.Request{
		Proto:     ipc.ProtoVersion,
		Script:    script,
		Cwd:       cwd,
		JSON:      jsonOut,
		StdinMode: ipc.StdinNone,
	}
	// opts 在进嵌套分支**之前**展开:Args 是脚本语义的一部分,嵌套里同样要绑
	// (见下面 runLocal 的调用);Fresh/Session 在嵌套里确实无事可做,但读进 req
	// 再忽略与"根本没展开"是两回事 —— 后者会让新增的 Option 静默失效。
	for _, o := range opts {
		o(&req)
	}
	if inSession() {
		// 嵌套时 runLocal 本来就每次新建 Session,天然是 fresh 的,Fresh/Session
		// 在这里无事可做;但位置参数必须带上 —— 修之前嵌套里的 `winbash -c 'echo $0' foo`
		// 把 foo 静默丢掉,与主路径是同一个 bug。
		return runLocal(script, req.Args, jsonOut, timeout)
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
	// daemon 已停,但端口文件未必跟着走:删它要 FILE_DELETE_CHILD / DELETE 权限,
	// 权限没给全时连 daemon 自己都删不掉自己写的文件(那正是「自定义 --endpoint
	// 永久不可用」缺陷的起点,见 daemon.removeIfOwned 与 restrictEndpointFile)。
	//
	// 这行提示只能由**这里**来说:后台 daemon 是 spawn 起来的,它的 stderr 在
	// daemonEnv / 命令行那一步就没接,它自己那句警告没人看得到;使用者眼前这个
	// 终端是唯一还能说话的地方。不改 --stop 的成败 —— 停是停成了,只是留了个尾巴。
	if info, rerr := readEndpoint(endpoint); rerr == nil {
		fmt.Fprintf(os.Stderr, "winbash: 警告: 端口文件 %s 还在(daemon 已停,但文件没删掉,"+
			"仍指向 %s),下次调用会自动重建 daemon\n", endpoint, info.addr)
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
	// Token 不在这里设:dial 可能因为旧地址连不上而顺手拉一个新 daemon,
	// 那时 token 会变。等 dial 返回「实际连上的是谁」之后再设(见下面)。
	if timeout > 0 {
		// daemon 侧也要知道上限:client 放弃不代表它该继续跑(见 ipc.Request.TimeoutMS)。
		req.TimeoutMS = int(timeout / time.Millisecond)
	}

	conn, used, err := dial(endpoint, info, mustExist, idle)
	if err != nil {
		return ipc.Response{}, err
	}
	// dial 可能换了 daemon(旧地址连不上,它顺手拉了一个新的)。连上哪个就用哪个的
	// token —— 否则会拿旧 token 去连新 daemon,必然 401(见 dial 的注释)。
	req.Token = used.token

	resp, xerr := exchange(conn, req, timeout)
	conn.Close()
	if xerr == nil {
		if resp.Auth {
			// 鉴权失败证明 daemon **没执行**请求,所以重发不会跑两遍。
			// 但带流式 stdin 时不能重发:第一条连接已经把 os.Stdin 消费掉了
			// (理由见 mayRetryAfterAuth)。
			if !mayRetryAfterAuth(req) {
				resp.Errors = append(resp.Errors,
					"client: 鉴权失败,但本次请求带流式 stdin(已被第一条连接消费),不能自动重发;"+
						"请重新运行这条命令(命令本身未执行)")
				return resp, nil
			}
			return retryAfterAuthFailure(endpoint, req, timeout, idle, mustExist)
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

	// ★ 只有**发送阶段**的失败才重试(2026-10-19 收窄)。
	//
	// 修之前这里对"非超时"一律重试,注释写的是「这种请求**极可能没送达**」。那句话
	// 对 `WriteMessage` 失败成立,对 `ReadMessage` 失败**不成立**:能走到读响应这一步,
	// 说明请求帧已经完整写出去了,daemon 可能已经读完、投进会话队列、甚至跑完了脚本。
	// 那时再重拉一个 daemon 原样重跑,就是同一条脚本跑两遍 —— 与上面「超时绝不重放」
	// 想消灭的是同一个失败形态,只是入口不同。
	//
	// 更要紧的是重放会打到**新** daemon 上(见下面的 ensureDaemonExcept),而旧 daemon
	// 可能还活着、脚本正在它身上跑;daemon 没有 Job Object,它派生的子进程也不会跟着
	// 退出 —— 于是新旧两个副本**并发**跑同一条脚本。
	//
	// 所以:发送失败 → 请求没送出去,重试安全;读响应失败 → 请求已送达,不重放,
	// 由调用方决定要不要重跑(错误文案里会说清"命令可能已执行")。
	//
	// 另外**确定性失败**(编码失败、超长帧)即使发生在发送阶段也不重试:
	// 它们再发一次还是同一个结果,而重试会先删端口文件、再 spawn 一个新 daemon
	// —— 为一个必然失败的原因干掉一个健康的 daemon,把旧的那个变成孤儿。
	if !isSendFailure(xerr) || isPermanentFailure(xerr) {
		return ipc.Response{}, xerr
	}

	// 剩下这类才是"半路上掉了":连接被重置、daemon 刚被杀。请求帧没能完整写出,
	// 所以 daemon 那边要么没读到、要么读到一半就放弃执行(daemon.handleConn 在
	// ReadMessage 失败时直接关连接)。重试一次是「从偶发竞态里恢复」和
	// 「不在真故障上打转」之间的中点。
	//
	// 但"发送失败"**不证明 daemon 死了**:daemon 侧每条连接有 5 秒读预算
	// (daemon.DefaultReadTimeout)读到一半就关连接,client 侧同样表现为写失败。
	// 修之前这里无条件「删端口文件 + 换新 daemon」,把这种还活着的旧 daemon 变成
	// 孤儿(--status 看不到、要等 idle 才退,可能还各抱一个 pwsh 引擎)—— 与
	// permanent 失败路径(上面那个 if,刻意不碰 daemon)的取舍正好相反。
	// 所以先对旧地址探活:活着就在它身上重发,确死才走换新的老路。
	if probeDaemon(info.addr) {
		// 活着:不 spawn。用手里留着的 addr+token 直接在旧 daemon 上重发。
		// 刻意**不**走 forgetStaleEndpoint —— 文件指向一个活着的 daemon,
		// 删它正好制造我们要避免的孤儿窗口;文件若已被别的进程删掉,原样写回
		// (见 restoreEndpoint),否则下一次调用读不到文件又会 spawn 一个新的。
		restoreEndpoint(endpoint, info)
		conn2, derr := net.DialTimeout("tcp", info.addr, dialTimeout)
		if derr == nil {
			req.Token = info.token
			resp2, xerr2 := exchange(conn2, req, timeout)
			conn2.Close()
			if xerr2 == nil {
				return resp2, nil
			}
			// 探活之后 daemon 恰好死了、或重发又碰上别的失败:门槛与第一次
			// 完全相同 —— 只有仍是**发送阶段**的瞬时失败才落回下面的换新路径
			// (请求依旧没送达,重发仍然安全);超时、读失败(可能已执行)、
			// permanent 都就地报错,绝不重放。
			if !isSendFailure(xerr2) || isPermanentFailure(xerr2) {
				return ipc.Response{}, xerr2
			}
		}
	}
	// 探活失败(真死了,连 listener 都没了)或活着重发又瞬时失败:走原有路径。
	forgetStaleEndpoint(endpoint, info.addr)
	// 同样要带 deadAddr:删不掉的残留文件会让 ensureDaemon 把死地址再交回来。
	info2, aerr := ensureDaemonExcept(endpoint, info.addr, idle)
	if aerr != nil {
		return ipc.Response{}, xerr
	}
	conn2, used2, derr := dial(endpoint, info2, false, idle)
	if derr != nil {
		return ipc.Response{}, xerr
	}
	defer conn2.Close()
	req.Token = used2.token
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
//
// mustExist 必须原样传下去:--stop / --status 的约定是「不为了管理操作把 daemon
// 拉起来」(见 Stop / Status 的注释)。修之前这里写死 false,于是 `--status` 恰好
// 撞上"读 token 后 daemon 退出"的竞态时,会顺手 spawn 一个新 daemon —— 管理命令
// 把一个使用者以为已经停了的东西重新启动了。
func retryAfterAuthFailure(endpoint string, req ipc.Request, timeout, idle time.Duration, mustExist bool) (ipc.Response, error) {
	info2, err := readEndpoint(endpoint)
	if err != nil {
		return ipc.Response{}, fmt.Errorf("client: daemon 鉴权失败,且端口文件已不可读: %w", err)
	}
	conn2, used, err := dial(endpoint, info2, mustExist, idle)
	if err != nil {
		return ipc.Response{}, err
	}
	defer conn2.Close()
	req.Token = used.token
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

// dial 建立到 info.addr 的连接,并返回**这次连接实际对应的**端点信息。
//
// 为什么返回值里要有 endpointInfo(修于 2026-10-19):旧地址连不上时,dial 会顺手
// 拉起一个新 daemon —— 那个 daemon 的 token 与调用方手里的**不一样**。而调用方
// 在 dial 之前就把 `req.Token = info.token` 设好了,于是它拿着旧 token 去连新 daemon,
// 必然 401。没带 stdin 时这条 401 还能被 retryAfterAuthFailure 救回来(多一次往返);
// 带流式 stdin 时救不回来(stdin 已被第一条连接消费),于是本来能成功的调用变成失败。
//
// 返回实际使用的 info,调用方据此覆盖 token,窗口就没了。
//
// mustExist=true 时不 spawn,返回值就是传进来的 info 本身。
func dial(endpoint string, info endpointInfo, mustExist bool, idle time.Duration) (net.Conn, endpointInfo, error) {
	conn, err := net.DialTimeout("tcp", info.addr, dialTimeout)
	if err == nil {
		return conn, info, nil
	}
	if mustExist {
		return nil, endpointInfo{}, fmt.Errorf("client: daemon 没在运行(端口文件 %s 指向的地址连不上)", endpoint)
	}
	// 端口文件指向的 daemon 已经不在了。清掉陈旧端口再拉一次。
	//
	// 这里必须把 addr 当 deadAddr 传下去:端口文件有可能删不掉(ACL 里缺 DELETE
	// 的残留文件),那样 forgetStaleEndpoint 是白删的,而 ensureDaemon 若照旧
	// "读到了就信任",拿回来的还是刚才连不上的同一个地址 —— 两条命令之间
	// 一步也没前进。带 deadAddr 进去,它才会真的重新 spawn 一个 daemon,
	// 由新 daemon 的 WriteFile 覆盖写那份残留文件。
	forgetStaleEndpoint(endpoint, info.addr)
	info2, aerr := ensureDaemonExcept(endpoint, info.addr, idle)
	if aerr != nil {
		return nil, endpointInfo{}, aerr
	}
	conn, err = net.DialTimeout("tcp", info2.addr, dialTimeout)
	if err != nil {
		return nil, endpointInfo{}, fmt.Errorf("client: 连接 daemon 失败: %w", err)
	}
	// 连上的是 info2 那个 daemon —— token 也必须换成它的。
	return conn, info2, nil
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
		// 删不掉必须**说出来**:这个文件指着已经连不上的地址,留着它就会让
		// 下一次调用先撞一次失败。原先 `_ = os.Remove(...)` 把它彻底吞掉,
		// 使用者只看到"连接失败",无从知道是哪个文件在挡路。
		// 恢复路径本身不会因为这次失败而失效(ensureDaemonExcept 会重新 spawn、
		// 由新 daemon 覆盖写这个文件),但静默是另一回事 —— 本项目不静默做错事。
		// NotExist 不算失败:目的(文件不在了)已经达到。
		if err := os.Remove(endpoint); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "winbash: 警告: 删不掉陈旧端口文件 %s(%v);"+
				"它会继续指向连不上的 %s,恢复要靠新建的 daemon 覆盖写它"+
				"(必要时手动删除,或 `icacls %s /grant:r <用户>:(R,W,D)`)\n",
				endpoint, err, addr, endpoint)
		}
	}
}

// probeTimeout 是发送失败后对旧 daemon 探活的 dial 上限。
//
// 比 dialTimeout 短:这只是「还听不听得见」的一问,问不出声就当死了。真死
// (端口没人听)在回环上立刻被拒,探活毫秒级返回;这个上限只为不会应答的地址
// (防火墙吞 SYN 之类)兜底,别让重试在它身上白等太久。
const probeTimeout = 2 * time.Second

// probeDaemon 对旧 daemon 的地址做一次 TCP 探活,报告它是否还在监听。
//
// 只 dial 不说话:daemon 对一条读不到请求的连接本来就是静默关掉(handleConn
// 在 ReadMessage 失败时 closeConnGracefully),所以探活连接不在 daemon 侧留下
// 任何状态,也不推进任何会话。发送失败 ≠ daemon 死了 —— daemon 侧的读预算
// (daemon.DefaultReadTimeout)读到一半关连接,client 侧同样是写失败;拿
// 「写失败」当「进程已死」去换新 daemon,活着的那个就成了孤儿。
func probeDaemon(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, probeTimeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// restoreEndpoint 在探活确认 daemon 活着之后,把缺失的端口文件用已知的
// addr+token 原样写回。
//
// 只在文件**读不出来**时写:文件还在但不指向我们这份内容,说明有新 daemon
// 已经接管(或另一个调用方正在写),覆盖写会把别人变成孤儿 —— 正是这次要
// 消灭的形态。写失败只警告不拦路:重发走的是手里的 addr+token,不依赖文件。
func restoreEndpoint(endpoint string, info endpointInfo) {
	if _, err := readEndpoint(endpoint); err == nil {
		return
	}
	if err := os.WriteFile(endpoint, []byte(info.addr+"\n"+info.token+"\n"), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "winbash: 警告: 端口文件 %s 写不回(%v);"+
			"旧 daemon 仍在 %s 上服务,但下一次调用会重建一个新 daemon\n",
			endpoint, err, info.addr)
	}
}

// sendFailure 标明这次失败发生在**请求写出**阶段。
//
// 它存在只为一件事:让 oneShot 能区分「请求没送出去」(可以安全重试)与
// 「请求送了、响应没读到」(命令可能已经执行,绝不能重放)。两者的底层错误
// (connection reset)长得一模一样,只能靠**发生位置**判定。
//
// permanent 是第二个维度:**重试有没有意义**。编码失败与超长帧同样是"一个字节
// 都没送出去",但它们重试必然再失败 —— 而重试路径会先删端口文件、再 spawn 一个
// 新 daemon,等于为了一个确定会失败的原因干掉一个健康的 daemon(旧 daemon 变成
// 孤儿,要等 idle 超时才退)。这类失败必须就地报错。
type sendFailure struct {
	err       error
	permanent bool
}

func (e *sendFailure) Error() string { return e.err.Error() }
func (e *sendFailure) Unwrap() error { return e.err }

// isSendFailure 报告失败是否发生在发送阶段(含编码失败 —— 那种情况一个字节都没出去)。
func isSendFailure(err error) bool {
	var sf *sendFailure
	return errors.As(err, &sf)
}

// isPermanentFailure 报告这次失败是不是「重试也不会好」。
//
// 只有发送阶段的失败才可能被标成 permanent(读响应阶段根本不该重试)。
func isPermanentFailure(err error) bool {
	var sf *sendFailure
	return errors.As(err, &sf) && sf.permanent
}

// mayRetryAfterAuth 报告鉴权失败后能否安全重发。
//
// **带了流式 stdin 就不能**:第一次 exchange 已经在往连接里灌 os.Stdin,鉴权失败
// 只说明 daemon 没执行命令,但 stdin 已经被消费掉了(可能全部)。重发会再起一支
// io.Copy 读同一个 fd —— 两个读取者瓜分同一条输入,第二个 daemon 拿到的是残缺
// 甚至空的 stdin,而命令照跑不误。那正是「静默给出错值」。
//
// 不带 stdin(StdinNone)时重发是安全的:daemon 的鉴权在 op 分派与执行**之前**
// (daemon.handleConn),401 时它只写回响应就关连接。
func mayRetryAfterAuth(req ipc.Request) bool {
	return req.StdinMode != ipc.StdinStream
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
		// 编码失败 = 一个字节都没发出去,语义上等同发送失败;
		// 但重发同一份 req 会得到同一个错误,所以是 permanent。
		return ipc.Response{}, &sendFailure{
			err:       fmt.Errorf("client: 编码请求失败: %w", err),
			permanent: true,
		}
	}
	if err := ipc.WriteMessage(conn, body); err != nil {
		if errors.Is(err, ipc.ErrPayloadTooLarge) {
			// 超长帧:连重试都没有意义,更不该为了它去停掉一个健康的 daemon。
			return ipc.Response{}, &sendFailure{
				err: fmt.Errorf("client: 发送请求失败: %w\n"+
					"         请求超过协议单帧上限,重试不会成功;请缩小脚本,或把它写成文件再执行", err),
				permanent: true,
			}
		}
		return ipc.Response{}, &sendFailure{err: fmt.Errorf(
			"client: 发送请求失败: %w\n"+
				"         命令**未执行**(请求没送达 daemon),可以安全重试", err)}
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
	//
	// 但它的**源端读错误**不能吞(修于 2026-10-19):stdin 是已关闭的文件、盘上
	// 读失败之类,会让命令拿到不完整输入却照样 exit 0 —— 正是本项目要消灭的形态。
	// 写端错误则静默:`yes | head -1` 这类正常用法里,head 退出后灌入端必然报
	// broken pipe,真 bash 在那里是被 SIGPIPE 静默杀掉的。
	var pump *stdinPump
	if req.StdinMode == ipc.StdinStream {
		pump = startStdinPump(conn, os.Stdin)
	}

	raw, err := ipc.ReadMessage(conn)
	if err != nil {
		// 走到这一行说明请求帧**已经完整写出**,daemon 可能已经执行了脚本。
		// 措辞必须说清这一点:使用者(或 AI)要据此决定是"放心重跑"还是
		// "先去确认副作用",而原先只有一句「读取响应失败」,什么都不敢做。
		return ipc.Response{}, fmt.Errorf("client: 读取响应失败: %w\n"+
			"         请求已送达 daemon,命令**可能已经执行**;请先确认副作用(文件、提交、进程)再决定是否重跑", err)
	}
	resp, derr := ipc.DecodeResponse(raw)
	if derr != nil {
		return ipc.Response{}, derr
	}
	// stdin 的源端错误作为一行警告带回去:命令本身可能已经成功,但它的输入
	// 不完整 —— 这件事必须让使用者知道,而不是让退出码替它说话。
	if perr := pump.err(); perr != nil {
		resp.Errors = append(resp.Errors, "client: "+perr.Error())
	}
	return resp, nil
}

// stdinPump 是一次流式 stdin 转发的句柄。
type stdinPump struct {
	done chan error
}

// startStdinPump 起一支 goroutine 把 src 灌进 conn。
//
// src 是参数而不是直接读 os.Stdin:这个函数的两种失败(源端读错、写端断掉)
// 语义相反,必须有回归 —— 而 os.Stdin 是全局的,没法在测试里换成可控的读者。
func startStdinPump(conn net.Conn, src io.Reader) *stdinPump {
	p := &stdinPump{done: make(chan error, 1)}
	go func() {
		// 包一层读端,只为**精确区分源端与目标端的错误**:io.Copy 把两者都返回
		// 成一个 error,而它们该不该报完全相反(见上面的注释)。
		rec := &recordingReader{r: src}
		_, copyErr := io.Copy(conn, rec)
		// 写完关写半侧:仍能读响应。Windows 上由 ws2_32.shutdown 实现,
		// 项目 daemon.Serve 收尾处已在用同一调用。
		if tc, ok := conn.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		switch {
		case rec.err != nil:
			// 源端读失败:输入真的不全,必须报。
			p.done <- fmt.Errorf("读取 stdin 失败,命令拿到的是不完整输入: %w", rec.err)
		default:
			// 其余一律静默,copyErr **和** CloseWrite 的错误都算。
			//
			// 为什么 CloseWrite 的失败也不能报:命令不读 stdin 时(例如
			// `winbash -c 'echo hi'` 配一个管道),daemon 写完响应就立刻
			// closeConnGracefully,而它的接收缓冲里还有没人读的 stdin ——
			// 那个 close 会发 RST,于是这边的 CloseWrite 报 WSAECONNRESET(10054)。
			// 这是一条**完全正常**的路径,响应早就拿到了;把它报成"stdin 转发
			// 收尾失败"只是在成功命令后面加一行噪音(复审当场指出,原先的
			// isConnGone 只认 net.ErrClosed,恰好漏掉 Windows 这一个)。
			//
			// 写端 broken pipe 同理:`yes | head -1` 里 head 退出后灌入端必然
			// 收到它,真 bash 在那里是被 SIGPIPE 静默杀掉的。
			_ = copyErr
			p.done <- nil
		}
	}()
	return p
}

// err 非阻塞地取一次转发结果。
//
// 为什么非阻塞:命令不读 stdin 而上游仍在灌时(README「流式 stdin」那节),
// ReadMessage 已经返回而 goroutine 还在流 —— 此时等它就是把一条正常命令
// 变成挂死。拿不到就当作"没有可报的错误",这正是既有行为。
func (p *stdinPump) err() error {
	if p == nil {
		return nil
	}
	select {
	case err := <-p.done:
		return err
	default:
		return nil
	}
}

// recordingReader 记下**源端**的读失败,供 stdin 转发结束后汇报。
type recordingReader struct {
	r   io.Reader
	err error
}

func (rr *recordingReader) Read(p []byte) (int, error) {
	n, err := rr.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		rr.err = err
	}
	return n, err
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

// trustEndpoint 判断「刚从端口文件读到的这一对地址/token 值不值得信任」。
//
// 抽成纯函数是为了能测(照同一个文件里 serveArgs 的先例):这里的两条分支正是
// 「删不掉的端口文件让 endpoint 永久不可用」那个缺陷的正中间 ——
// 只要照旧无条件信任,ensureDaemon 就会把刚刚连不上的**同一个**地址原样交回去,
// 重试多少次都在原地打转(实测:`--stop` 之后文件残留,之后每条命令都是"连接失败",
// 而且永远如此)。
//
// 两种不信任:
//   - err != nil:压根没读到(文件不存在 / 空文件),没什么可信任的;
//   - addr == deadAddr:这正是刚刚连不上的那个地址。**文件还在不等于 daemon 还在**,
//     残留的端口文件恰恰是这次故障的形态,所以这里必须换一个 daemon,
//     而不是再信它一次。
//
// deadAddr 为空表示「没有已知的坏地址」(首次启动、或 --stop/--status 这类管理操作),
// 这时读到什么都算可信。
func trustEndpoint(info endpointInfo, err error, deadAddr string) bool {
	if err != nil {
		return false
	}
	if deadAddr != "" && info.addr == deadAddr {
		return false
	}
	return true
}

// ensureDaemon 返回端口文件内容;daemon 不在就先把它拉起来。
//
// idle 会原样转给新起的 daemon。**这件事必须做到**:不转发的话,使用者写的
// `--idle 3s` 会被静默丢弃,自动拉起的 daemon 永远用默认的 30 分钟 ——
// 属于"文档承诺了、实现没转发",而这类落差最伤信任(实测:不转发时等 6s 后
// daemon 仍在、端口文件仍在)。
func ensureDaemon(endpoint string, idle time.Duration) (endpointInfo, error) {
	return ensureDaemonExcept(endpoint, "", idle)
}

// ensureDaemonExcept 同 ensureDaemon,但**不信任**端口文件里指向 deadAddr 的那份内容。
//
// 为什么需要它(缺陷现场):端口文件可能因为权限不足而删不掉 —— restrictEndpointFile
// 曾经只授 (R,W)、没有 D,于是 daemon 退出时删不掉自己写的文件。那种残留文件永远
// 指着同一个已经没人监听的地址,而 ensureDaemon 每轮都"读到了就信任",于是
// 「连不上 → 删文件(失败) → 又信任同一个死地址 → 连不上」永不收敛。
//
// 带 deadAddr 进来之后,每一处「读到了就信任」都过 trustEndpoint 这一关;
// 一旦那次读到的仍是死地址,就走到 spawnDaemon:新 daemon 的 os.WriteFile 会
// 覆盖写同一个文件(ACL 里 W 仍在),于是残留文件被改写成新地址,自愈。
func ensureDaemonExcept(endpoint, deadAddr string, idle time.Duration) (endpointInfo, error) {
	if info, err := readEndpoint(endpoint); trustEndpoint(info, err, deadAddr) {
		return info, nil
	}

	// 从这里往下是「没有可用的 daemon,要拉起一个」——必须跨进程互斥。
	//
	// 不互斥会怎样(实测):8 个 client 同时首次调用,每个都读到「没有 daemon」,
	// 于是各 spawn 一个。daemon 用 127.0.0.1:0 取随机端口,所以**它们全都绑定成功**,
	// 不存在"后来者失败退出"这种天然去重 —— 结果 8 个 --serve 进程同时活着,
	// 端口文件只指向最后写入的那一个,其余 7 个是孤儿:--status 看不到、--stop 停不掉,
	// 各自还抱着一个 Session(用到 ② 层的还各抱一个 pwsh)白占内存,要等 30 分钟空闲才退。
	//
	// 为什么是个循环:alreadyUp 那条路上读到的端口文件**也可能**正指着 deadAddr
	// (持锁者同样删不掉那个残留文件,甚至持锁者就是修复前的老版本)。那种时候
	// 信任它就是原样回到缺陷里,所以不返回、重抢锁,直到我们自己拿到锁去 spawn。
	deadline := time.Now().Add(startupBudget)
	for {
		release, alreadyUp, err := acquireStartLock(endpoint)
		if err != nil {
			return endpointInfo{}, err
		}
		if alreadyUp {
			// 等锁的那段时间里,持锁者已经把 daemon 起来了 —— 但**只有当它起来的是
			// 个新的**才算数(见 trustEndpoint)。
			if info, rerr := readEndpoint(endpoint); trustEndpoint(info, rerr, deadAddr) {
				return info, nil
			}
			// 仍是那个死地址。不信任它,但也不能在没有锁的情况下 spawn
			// (那正是上面 8 个 daemon 的来路),所以退回去重抢。alreadyUp 时
			// release 是 nil,这里不会漏掉一把锁。
			//
			// 必须 sleep + 走总预算:acquireStartLock 在「文件存在」时会立刻返回
			// alreadyUp,不 sleep 就是纯忙等,而它自己的超时又被这个分支绕过了。
			if time.Now().After(deadline) {
				return endpointInfo{}, fmt.Errorf(
					"client: daemon 在 %s 内没有起来:端口文件 %s 仍指向连不上的 %s。"+
						"若确实没有别的 winbash 在跑,删掉这个端口文件再试", startupBudget, endpoint, deadAddr)
			}
			time.Sleep(pollInterval)
			continue
		}

		// 抢到锁之后要**再读一次**:上一个持锁者可能刚好在放锁之前写好了端口文件。
		// 少了这一次,我们就会在一个已经健康的 daemon 旁边再起一个 —— 正是要消灭的那件事。
		// 但同样是「读到了就信任」,所以照样要过 trustEndpoint:残留的死地址不算数。
		if info, rerr := readEndpoint(endpoint); trustEndpoint(info, rerr, deadAddr) {
			release()
			return info, nil
		}
		if serr := spawnDaemon(endpoint, idle); serr != nil {
			release()
			return endpointInfo{}, fmt.Errorf("client: 启动 daemon 失败: %w", serr)
		}
		// 新 daemon 的 os.WriteFile 会**覆盖写**那个删不掉的残留文件(ACL 里的 W 还在),
		// 所以下面等的不是"文件出现",而是"文件里出现一个不是 deadAddr 的地址"。
		spawnDeadline := time.Now().Add(startupBudget)
		for time.Now().Before(spawnDeadline) {
			if info, rerr := readEndpoint(endpoint); trustEndpoint(info, rerr, deadAddr) {
				release()
				return info, nil
			}
			time.Sleep(pollInterval)
		}
		release()
		return endpointInfo{}, fmt.Errorf("client: daemon 在 %s 内没有起来", startupBudget)
	}
}

// startLockPath 是与端点配套的启动锁文件路径。
//
// 放在端口文件旁边(同一个目录),所以端点隔离到哪、锁就隔离到哪 —— 两个跑在不同
// --endpoint 上的测试不会互相挡。
func startLockPath(endpoint string) string { return endpoint + ".lock" }

// parseStartLock 解析锁文件的内容:写入处(acquireStartLock 建锁时)留下的两行
// 「pid\n纳秒时间戳」。除了 pid 还要带时间戳,是因为接管者重建的锁 pid 可能相同
// (同一测试进程里先后 acquire 就是同 pid),时间戳行才是"这是我写下那把"的凭据。
func parseStartLock(b []byte) (pid, stamp int64, ok bool) {
	pidLine, rest, _ := strings.Cut(string(b), "\n")
	stampLine, _, _ := strings.Cut(rest, "\n")
	p, err1 := strconv.ParseInt(pidLine, 10, 64)
	s, err2 := strconv.ParseInt(stampLine, 10, 64)
	return p, s, err1 == nil && err2 == nil
}

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
			pid, stamp := int64(os.Getpid()), time.Now().UnixNano()
			// 写 pid 与时间戳:除了事后排查"这把锁是谁留下的",release 还要靠
			// 回读这两行确认锁还在自己手里。
			_, _ = fmt.Fprintf(f, "%d\n%d\n", pid, stamp)
			_ = f.Close()
			return func() {
				// 删之前回读:持锁期间本进程可能被暂停超过 staleLockBudget,锁已被
				// 别人接管重建 —— 那再删就是拔掉接管者的锁,给第三个进程并发
				// spawn 开窗。只有内容仍是自己写下的那两行(pid + 时间戳)才删;
				// 读不到或格式不认识时保守不删,宁可留一把待接管的陈锁,不误删活锁。
				if b, rerr := os.ReadFile(path); rerr == nil {
					if p, s, ok := parseStartLock(b); ok && p == pid && s == stamp {
						_ = os.Remove(path)
					}
				}
			}, false, nil
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
func runLocal(script string, args []string, jsonOut bool, timeout time.Duration) (ipc.Response, error) {
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
	out, exit, errs := sess.RunArgs(ctx, script, args, jsonOut)
	if cerr := ctx.Err(); cerr != nil {
		// os.ErrDeadlineExceeded 带 Timeout() 方法,与 daemon 路径的超时是同一类,
		// 上游能一眼认出"这是超时,不是脚本本身失败"。
		return ipc.Response{}, fmt.Errorf("嵌套执行超过 %s: %w", timeout, os.ErrDeadlineExceeded)
	}
	return ipc.Response{OK: exit == 0, Out: []byte(out), Errors: errs, Exit: exit}, nil
}
