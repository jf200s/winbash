// Package daemon 是 winbash 的常驻服务端。
//
// 传输层用 TCP loopback 而不是命名管道：Go 标准库没有 CreateNamedPipe 封装，
// 而项目只允许一个直接依赖(mvdan/sh)。127.0.0.1 仅本机可达，对本机开发工具可接受。
//
// 但「仅本机可达」不等于「只有你连得上」—— 同机**其他用户**与沙箱进程同样连得上。
// 所以端口文件是两行(地址 + 随机 token),每个请求都要带对 token(见 ipc.Request.Token)。
package daemon

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"winbash/internal/ipc"
	"winbash/internal/shell"
)

const createNoWindow = 0x08000000 // CREATE_NO_WINDOW

const (
	// maxSessions 是**具名会话**数上限。默认会话不占这个名额(理由见 pickSession)。
	// 超了拒绝,不回收最久未用的 —— 回收等于静默销毁状态,而状态正是这条会话存在的理由。
	maxSessions = 16
	// stopGrace 是 --stop 给「正在跑的请求」的宽限。到点仍在跑的会被取消。
	stopGrace = 5 * time.Second
)

// DefaultReadTimeout 是一条连接**读完请求**的预算,可由 Serve 覆盖。
//
// 为什么必须有:ipc.ReadMessage 会一直等对方把帧发完。一个只连上、一个字节都不发的
// 连接会一直挂着(以前它还会把整个 daemon 卡住 —— 那时 handle 是同步串行调用的,
// 后续每条命令都排在它后面,实测要等到 client 自己的超时(默认 300s)才散场)。
// 现在连接各自一个 goroutine,所以它只拖住自己;预算仍然要有,否则一条这样的连接
// 会让 daemon 白占一个 goroutine 与一个 fd,直到对端消失。
//
// 为什么是 5s 这么"紧":这是 loopback,合法请求在连上之后**立刻**就发出来了(微秒级)。
// 实测用 30s 时,一个静默连接仍能让 daemon 沉默半分钟 —— 机制对,但代价还是太高。
// 5s 对任何真实客户端都远远够用,同时把"被一个坏连接拖住"的上限压到可接受。
const DefaultReadTimeout = 5 * time.Second

// job 是一次排队等某个会话执行的请求。
type job struct {
	req  ipc.Request
	conn net.Conn
}

// sessionEntry 是一个具名会话:一个独立 Session + 一条队列 + 一个 worker。
//
// 为什么不只用一把 mutex:mutex **不保证 FIFO**,同一会话里 `X=1` 与 `echo $X`
// 一旦乱序就是静默的状态损坏。队列 + 单 worker 天然严格 FIFO 且串行。
type sessionEntry struct {
	name     string
	sess     *shell.Session
	jobs     chan job
	lastUsed time.Time

	// workerDone 在 worker 真的退出后关闭 —— 关 Session 之前要等它,
	// 否则会与一次正在执行的 Run 撞上(见 closeSessions)。
	workerDone chan struct{}

	// busy 表示这条会话此刻有一个 job 在跑(受 daemon.mu 保护)。
	busy bool
	// snapDir / snapPwsh 是上一次 job 结束时的状态快照。
	// 为什么要快照:--status 不再排队,它会与 job **并发**执行,而 shell.Session 明确
	// 不并发安全(见 Session 的注释)—— busy 时直接读 Dir()/EngineStatus() 就是数据竞争。
	snapDir  string
	snapPwsh string
}

// daemon 持有全部跨连接状态。
type daemon struct {
	ln       *net.TCPListener
	addr     string // Accept 前存下来,shutdown 里还要用它写 removeIfOwned
	token    string
	endpoint string
	idle     time.Duration
	started  time.Time // 启动时刻,空闲退出的起点

	mu       sync.Mutex
	sessions map[string]*sessionEntry // key 是会话名,空串 = 默认会话
	running  int                      // 正在执行的 job 数(受 mu 保护)

	stopOnce sync.Once
	done     chan struct{} // shutdown 开始时关闭
	stopCtx  context.Context
	stopStop context.CancelFunc

	// stopAcceptOnce 保证「关监听 + 删端口文件」只做一次(幂等)。
	stopAcceptOnce sync.Once
	// shutdownDone 在 runShutdown **全部收尾完成**后关闭。
	//
	// Serve 在 Accept 报错时要等它:main 一看到 Serve 返回就 return、进程随即退出,
	// 若那时会话还没关完,后台清理会被进程退出打断,② 层的 pwsh 变成孤儿进程。
	shutdownDone chan struct{}
}

// Serve 在 endpoint(端口文件路径)上启动服务并阻塞,直到被 --stop 或空闲超时结束。
//
// readTimeout 是单条连接读请求的预算(<=0 用 DefaultReadTimeout)。
//
// 退出时(两种情况都算)必须删除端口文件 —— 否则下次 client 会读到陈旧端口、
// 连上一个不存在的地址。
func Serve(endpoint string, idleTimeout, readTimeout time.Duration) error {
	if readTimeout <= 0 {
		readTimeout = DefaultReadTimeout
	}
	token, err := newToken()
	if err != nil {
		return fmt.Errorf("daemon: 生成 token: %w", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer ln.Close()

	// 端口文件两行:地址 + token。写在一起是因为它们必须同时生效 ——
	// 只发一个地址,就是修之前那个「任意本机进程都能借 daemon 身份执行命令」的洞。
	if err := os.WriteFile(endpoint, []byte(ln.Addr().String()+"\n"+token+"\n"), 0o600); err != nil {
		return fmt.Errorf("daemon: 写端口文件失败: %w", err)
	}
	defer removeIfOwned(endpoint, ln.Addr().String())
	restrictEndpointFile(endpoint)

	stopCtx, stopStop := context.WithCancel(context.Background())
	d := &daemon{
		ln:       ln.(*net.TCPListener),
		addr:     ln.Addr().String(),
		token:    token,
		endpoint: endpoint,
		idle:     idleTimeout,
		started:  time.Now(),
		sessions: make(map[string]*sessionEntry),
		done:     make(chan struct{}),
		stopCtx:  stopCtx,
		stopStop: stopStop,

		shutdownDone: make(chan struct{}),
	}
	// 兜底:shutdown 通常已经把会话收干净了(shutdown 与这里走同一个 takeSessions,
	// 谁先摘到谁负责关),但 watchdog/Accept 报错等路径仍可能抢在它前面让 Serve 返回。
	// 会话持有 ② 层的常驻 pwsh 与 ① 层的状态,退出时一定要收干净 ——
	// 否则每重启一次 daemon 就留下一个 pwsh 进程。
	defer func() {
		d.closeAllSessions()
		stopStop()
	}()

	go d.watchdog()

	for {
		// 不再在 Accept 上设 idle deadline:并发之后 Accept 返回不代表「空闲」——
		// 一条长命令正在某个会话里跑的时候,Accept 照样在等新连接。空闲判定搬进
		// watchdog(见那里),它看的是「有没有 job 在跑 + 每条会话有多久没被人用过」。
		conn, err := d.ln.Accept()
		if err != nil {
			// --stop 与空闲退出都是先关监听再走这里,所以 d.done 已关闭时那不是故障,
			// 是**要求**我们退出。这个判断不能省:直接 return err 会让 main 把一次
			// 正常的 --stop 打印成 daemon 崩溃。
			//
			// pwshx 上踩过的 bug 正是"只设了标志、外层循环又重建监听",
			// 导致 --stop 静默失效 —— 所以这里绝不回到循环顶部。
			select {
			case <-d.done:
				// 等清理收尾完成再返回:main 一看到 Serve 返回就退进程,
				// 早走会让 closeSessions 被打断、留下孤儿 pwsh。
				<-d.shutdownDone
				return nil
			default:
			}
			return err
		}

		// 每条连接一个 goroutine:并发是这条分支的核心目的(不同会话之间并行)。
		// 同一会话内部仍然严格串行 —— 由该会话自己的队列 + worker 保证(见 sessionEntry)。
		go d.handleConn(conn, readTimeout)
	}
}

// watchdog 代替原来的 Accept deadline 做空闲退出。
//
// 不能用「Accept 超时」当空闲判据:Accept 的等待与新连接数无关,而一个正在跑的
// 长命令期间可能完全没有新连接 —— 旧逻辑会把一个正忙的 daemon 判成空闲。
// 这里三个条件都看:距启动已过 idle、没有 job 在跑、并且每条会话都超过 idle
// 没人用过。
func (d *daemon) watchdog() {
	if d.idle <= 0 {
		return
	}
	tick := d.idle / 10
	if tick < time.Second {
		tick = time.Second
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-d.done:
			return
		case <-t.C:
			d.mu.Lock()
			busy := d.running > 0
			// 空闲起点是**启动时刻**:一个刚起来、还没人用过的 daemon 不算空闲。
			// 少了这一条,它在 idle/10 之后就会自判空闲退出 —— 默认 idle=30min 时
			// 那是 3 分钟,而原来的语义(在 Accept 上挂 idle deadline)是「启动后
			// 30 分钟没人连才退」。常驻带来的加速(尤其 ② 层 pwsh 的冷启动)就
			// 因为这条差一而被白丢。
			idle := time.Since(d.started) >= d.idle
			for _, e := range d.sessions {
				if time.Since(e.lastUsed) < d.idle {
					idle = false
					break
				}
			}
			d.mu.Unlock()
			if !busy && idle {
				d.shutdown(0)
				return
			}
		}
	}
}

// shutdown 请求 daemon 退出。幂等,且立刻返回 —— 关停本身在后台做。
//
// 为什么必须异步:--stop 的那条连接要先读到确定答复(见 handleConn),它绝不能等
// 自己触发的关停做完再返回,否则 client 会在等答复时被自己的 shutdown 撞上。
func (d *daemon) shutdown(grace time.Duration) {
	d.stopOnce.Do(func() {
		// 关 done 要在**最前面**:handleConn 的投递、serveSession 的 select、
		// watchdog 的循环都以它为准。
		close(d.done)
		// 立刻关监听 + 删端口文件,而不是等到清理末尾 —— 这一步决定的是
		// 「外部还看不看得见这个 daemon」:
		//   --stop 先关它再回 OK,于是 client 拿到 OK 后紧接着的 --status 必然失败;
		//   原先把它排在会话清理之后,那之间有一个窗口(最长等于 grace + 一个
		//   Session.Close 的耗时),窗口里 --status 仍能成功 —— 测试
		//   TestStatusThenStopThenStatusFails 挂在这儿。
		d.stopAccepting()
		go d.runShutdown(grace)
	})
}

// stopAccepting 让 daemon 立刻从外部"消失":关监听 + 删端口文件。幂等。
//
// 顺序上必须在回 OK 之前(见 shutdown 的注释);它只影响**新**连接,
// 已经 Accept 的连接(包括正在回 OK 的这一条)不受影响。
func (d *daemon) stopAccepting() {
	d.stopAcceptOnce.Do(func() {
		_ = d.ln.Close()
		// 紧接着来的 client 若读到还在的端口文件,就会连上一个**正在退出**的监听 ——
		// 实测表现为 "An existing connection was forcibly closed by the remote host"。
		// 删掉它,后来的 client 就会去拉一个新 daemon(那才是它想要的)。
		removeIfOwned(d.endpoint, d.addr)
	})
}

func (d *daemon) runShutdown(grace time.Duration) {
	defer close(d.shutdownDone)

	// grace > 0 时给正在跑的请求一点时间自己收尾(--stop 的正常语义:让你手上这条
	// 命令能给出结果,而不是被拦腰砍断)。到点还在跑的一律取消 —— 无限循环不能
	// 让 --stop 变成"停不下来"。
	if grace > 0 {
		deadline := time.Now().Add(grace)
		for time.Now().Before(deadline) {
			d.mu.Lock()
			n := d.running
			d.mu.Unlock()
			if n == 0 {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	// 取消所有在跑的 job(它们的 ctx 从 stopCtx 派生)。
	d.stopStop()

	// 摘走会话表:从此 pickSession 只能报「正在退出」,不会再有新 job 进来。
	entries := d.takeSessions()

	// 给 worker 一点时间从 select 里退出来并把队列里剩下的请求答复掉
	// (见 serveSession:它有 d.done 分支,退出前会把排队中的请求明确拒掉)。
	time.Sleep(100 * time.Millisecond)

	// 关 Session 不持锁:Close 会杀 ② 层的 pwsh,可能慢,也可能因为 handler 正在用
	// 它而等一会儿 —— 持锁做这件事会把 --status 一起卡死。
	wait := grace
	if wait < time.Second {
		// 至少留一点时间等 worker 退出:与一次正在执行的 Run 并发地 Close 会话是
		// 数据竞争(shell.Session 不并发安全),而这里通常几毫秒就等到。
		wait = time.Second
	}
	d.closeSessions(entries, wait)
}

// takeSessions 把会话表摘空并返回一份副本。
//
// 摘走(而不是只读)是关键:关停路径有两条(shutdown 与 Serve 的 defer),谁先摘到
// 谁负责关 —— 于是同一个 Session 绝不会被两次 Close(那会与它内部对 engine 的
// 赋值形成数据竞争),摘不到的那条路径就什么都不做。
func (d *daemon) takeSessions() []*sessionEntry {
	d.mu.Lock()
	defer d.mu.Unlock()
	entries := make([]*sessionEntry, 0, len(d.sessions))
	for name, e := range d.sessions {
		entries = append(entries, e)
		delete(d.sessions, name)
	}
	return entries
}

// closeAllSessions 是 Serve 的兜底清理:shutdown 没走到时也要把 pwsh 收干净。
func (d *daemon) closeAllSessions() {
	d.closeSessions(d.takeSessions(), 0)
}

// closeSessions 关闭一批 Session,但先等各自的 worker 退出来。
//
// 为什么必须等:worker 正在 Run 的时候去 Close 会话,就是与它对 interp.Runner /
// engine 的读写并发 —— 而 shell.Session 明确不并发安全。正常路径上 worker 早就被
// d.done 通知退出了,等待是零成本的;真等不到(budget 到点)才强行关:那时宁可
// 让那一个 pwsh 随进程一起消失,也不能让一个还在跑的 handler 攥着一个半死的会话。
//
// budget<=0 时只做一次非阻塞检查(兜底路径不拖慢退出)。
func (d *daemon) closeSessions(entries []*sessionEntry, budget time.Duration) {
	deadline := time.Now().Add(budget)
	for _, e := range entries {
		d.waitWorker(e, deadline)
		_ = e.sess.Close()
	}
}

// waitWorker 等一个会话的 worker 退出。
func (d *daemon) waitWorker(e *sessionEntry, deadline time.Time) {
	for {
		select {
		case <-e.workerDone:
			return
		default:
		}
		if !time.Now().Before(deadline) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// handleConn 处理一条连接:读请求 → 鉴权 → op 分派 → 投递到会话队列。
//
// 这个函数**只做不阻塞的事**:真正的脚本执行在会话 worker 里(见 runJob)。
// 所以一条卡住的连接、一条长命令都不再能拖住别人。
func (d *daemon) handleConn(conn net.Conn, readTimeout time.Duration) {
	// 先挂上读预算再去读请求:没有它,一个只连不说活的连接会白占一个 goroutine
	// 与一个 fd,直到对端消失(见 DefaultReadTimeout)。
	if readTimeout > 0 {
		_ = conn.SetReadDeadline(time.Now().Add(readTimeout))
	}
	body, err := ipc.ReadMessage(conn)
	if err != nil {
		closeConnGracefully(conn)
		return
	}
	// 请求已经读完,撤掉读预算:执行期间我们不从这条连接读东西,留着一个过期的
	// deadline 只会在以后改成"边跑边读"时咬人。
	_ = conn.SetReadDeadline(time.Time{})

	req, err := ipc.DecodeRequest(body)
	if err != nil {
		// 单个连接的坏帧不该打死整个 daemon,也不该让这条连接永远挂着。
		closeConnGracefully(conn)
		return
	}

	// 鉴权放在 op 分派**之前**:stop / status 同样是能力,不能让外人用
	// (实测裸 socket 发 {"op":"stop"} 就能把别人的 daemon 打停)。
	if !tokenOK(req.Token, d.token) {
		_ = writeResponse(conn, ipc.Response{
			OK:   false,
			Auth: true,
			Errors: []string{"daemon: 鉴权失败。token 随端口文件一起生成,只有本用户的 winbash client 读得到它;" +
				"若你确是本用户,重跑一次命令即可(会连上新 daemon)"},
			Exit: 1,
		})
		closeConnGracefully(conn)
		return
	}

	// op 分派先做,而且**不经会话队列** —— 否则 --status/--stop 会被一条长命令
	// 饿死(以前靠 client 侧 5s 上限扛着,并发后应该真的不排队)。
	switch req.Op {
	case "stop":
		// **先关停、再回 OK**,顺序不能反:shutdown 里第一件事就是关监听 +
		// 删端口文件,于是 client 拿到 OK 时 daemon 已经"消失"了。反过来做会留
		// 一个窗口,窗口里紧接着的 `--status` 仍能连上 —— 实测就是这么挂的
		// (TestStatusThenStopThenStatusFails)。
		//
		// 三步都是非阻塞的:shutdown 起 goroutine 去做清理,回 OK 与关连接都立刻完成。
		// 正在跑的命令由 runShutdown 给 stopGrace 宽限,不会因为这条 --stop 被砍断。
		d.shutdown(stopGrace)
		_ = writeResponse(conn, ipc.Response{OK: true, Exit: 0})
		closeConnGracefully(conn)
		return

	case "status":
		_ = writeResponse(conn, ipc.Response{OK: true, Out: []byte(d.statusText(conn))})
		closeConnGracefully(conn)
		return
	}

	// 协议版本只对**执行脚本**这条路检查,并且刻意放在 op 分派之后。
	//
	// 两个理由,缺一个都会咬人:
	//  1. 必须放行 stop —— 否则新版 client 无法停掉旧 daemon,而停掉它正是版本不匹配
	//     时的推荐处置,于是陷入「要停它得先连上它」的死锁。
	//  2. 只有执行脚本这条路的语义会随版本漂移(比如 stdin 的有无),stop/status 的
	//     语义是稳定的。
	if err := ipc.CheckProto("daemon", req.Proto); err != nil {
		_ = writeResponse(conn, ipc.Response{
			OK:     false,
			Errors: []string{"daemon: " + err.Error()},
			Exit:   1,
		})
		closeConnGracefully(conn)
		return
	}

	// 空串就是默认会话,与引入 --session 之前完全等价。
	e, err := d.pickSession(req.Session)
	if err != nil {
		_ = writeResponse(conn, ipc.Response{
			OK:     false,
			Errors: []string{"daemon: " + err.Error()},
			Exit:   1,
		})
		closeConnGracefully(conn)
		return
	}

	// 投递到该会话的队列。带 d.done 分支:daemon 正在退出时立刻给出确定答复,
	// 而不是把这条连接留在队列里等它自己的超时。
	select {
	case <-d.done:
		_ = writeResponse(conn, ipc.Response{
			OK:     false,
			Errors: []string{"daemon: daemon 正在退出"},
			Exit:   1,
		})
		closeConnGracefully(conn)
	case e.jobs <- job{req: req, conn: conn}:
	}
}

// pickSession 取(没有就建)一条具名会话。
//
// 加锁做两件事:查表/建表,以及刷新 lastUsed(空闲退出的依据)。
// NewSession 只是构造 runner 与搜索路径,不派生任何进程,所以在锁里建是可以的;
// ② 层的 pwsh 是懒启动的,真正的开销在 Run 里。
func (d *daemon) pickSession(name string) (*sessionEntry, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	select {
	case <-d.done:
		return nil, fmt.Errorf("daemon 正在退出")
	default:
	}

	if e, ok := d.sessions[name]; ok {
		e.lastUsed = time.Now()
		return e, nil
	}

	// 到上限就拒绝,不回收最久未用的那条:回收等于**静默销毁状态**,而状态正是
	// 这条会话存在的理由。宁可让使用者看到一条明确的错误。
	//
	// 只数**具名**会话:默认会话(空名)不计入。理由是它不是一个"并行工作流",
	// 而是所有不指定会话名的调用共享的那一个,而且它总存在 —— 计入的话使用者会
	// 莫名少一个槽(写 16 个会话名却只能开 15 个),错误信息里的数字也就对不上。
	if name != "" {
		named := 0
		for k := range d.sessions {
			if k != "" {
				named++
			}
		}
		if named >= maxSessions {
			return nil, fmt.Errorf("具名会话数已达上限 %d(每个会话一个独立 shell 会话,"+
				"用到 ② 层时还会各持一个常驻 pwsh)。\n怎么办: 先 `winbash --stop` 清空,"+
				"或者复用已有的会话名", maxSessions)
		}
	}

	sess := shell.NewSession()
	e := &sessionEntry{
		name: name,
		sess: sess,
		// 队列容量 64:满了就在投递处背压,不静默丢弃。
		jobs:       make(chan job, 64),
		lastUsed:   time.Now(),
		workerDone: make(chan struct{}),
		// 建会话这一刻没有任何 job 在跑,是安全读 Session 字段的时机之一。
		snapDir:  sess.Dir(),
		snapPwsh: sess.EngineStatus(),
	}
	d.sessions[name] = e
	go d.serveSession(e)
	return e, nil
}

// serveSession 是会话的 worker:严格 FIFO、逐条执行。
//
// 为什么不用一把 mutex 串行整条会话:mutex 不保证先来先服务,同一会话里
// `X=1` 与 `echo $X` 一旦乱序就是静默的状态损坏(见 sessionEntry)。
//
// 退出条件只有 d.done(队列**不**会 close):关闭一个还有人在 select 里投递的
// channel 会 panic —— "select 里有 done 分支"挡不住这一点,两个 case 同时就绪时
// 运行时会随机选,选中 send 分支就是 send on closed channel。所以关停改用 done 通知。
func (d *daemon) serveSession(e *sessionEntry) {
	defer close(e.workerDone)
	for {
		select {
		case <-d.done:
			// 退出前把已经排队、但还没轮到的请求明确拒掉:留着它们只会让对应
			// 的 client 干等自己的超时。
			d.rejectQueued(e)
			return
		case j := <-e.jobs:
			d.runJob(e, j)
		}
	}
}

// rejectQueued 把队列里剩下的请求逐个答复成「正在退出」。
func (d *daemon) rejectQueued(e *sessionEntry) {
	for {
		select {
		case j := <-e.jobs:
			_ = writeResponse(j.conn, ipc.Response{
				OK:     false,
				Errors: []string{"daemon: daemon 正在退出"},
				Exit:   1,
			})
			closeConnGracefully(j.conn)
		default:
			return
		}
	}
}

// runJob 在一个会话里执行一条请求。同一会话的 runJob 之间严格串行(单 worker)。
func (d *daemon) runJob(e *sessionEntry, j job) {
	conn := j.conn
	req := j.req

	d.mu.Lock()
	d.running++
	e.busy = true
	e.lastUsed = time.Now()
	d.mu.Unlock()
	defer func() {
		// 快照在锁外取(它可能碰到 ② 层连接的内部锁),但必须在这个 goroutine 里取:
		// 同会话只有一个 worker,此刻没有任何人正用这条 Session —— 这是唯一能安全读
		// 它字段的时刻。取完再进锁写回 entry,给 --status 用(见 statusText)。
		dir, pwsh := e.sess.Dir(), e.sess.EngineStatus()
		// 收工时再刷一次 lastUsed:空闲判据要按"最后一次用完"算,否则一条跑了
		// 半小时的命令一走完,daemon 会立刻被判成空闲 —— 而 client 可能正要接着用。
		d.mu.Lock()
		d.running--
		e.busy = false
		e.lastUsed = time.Now()
		e.snapDir, e.snapPwsh = dir, pwsh
		d.mu.Unlock()
	}()

	// 失败路径的公共出口:写一条确定答复再优雅关连接(文案由调用方给全)。
	fail := func(exit int, msg string) {
		_ = writeResponse(conn, ipc.Response{
			OK:     false,
			Errors: []string{msg},
			Exit:   exit,
		})
		closeConnGracefully(conn)
	}

	// ctx 从 stopCtx 派生:--stop 要能取消在跑的请求,而 client 放弃**也不代表**
	// daemon 该继续跑 —— 一条无限循环会一直占着这条会话,把同一会话里后续所有命令
	// 饿死(实测第二条命令要等满自己的超时)。所以再叠一层请求自带的超时。
	ctx, cancel := context.WithCancel(d.stopCtx)
	defer cancel()
	if req.TimeoutMS > 0 {
		var c2 context.CancelFunc
		ctx, c2 = context.WithTimeout(ctx, time.Duration(req.TimeoutMS)*time.Millisecond)
		defer c2()
	}

	// note 收集"不影响主流程、但使用者必须知道"的错误行。
	var note []string

	// fresh 请求跑在**一次性会话**里:新建一个,runJob 返回时 Close 掉。长驻的
	// e.sess 一格都不动 —— 所以它既不受先前命令影响,也不把影响留给后面的命令
	// (语义与代价见 ipc.Request.Fresh)。Close 是必须的:② 层的常驻 pwsh 引擎挂在
	// Session 上,漏掉 Close 就漏一个进程。
	//
	// 注意它**仍然排在这条会话的队列里**(runJob 本来就是被 worker 串行调用的):
	// `--fresh` 的串行语义与引入会话池之前完全一致,默认行为零变化。
	execSess := e.sess
	if req.Fresh {
		execSess = shell.NewSession()
		defer func() { _ = execSess.Close() }()
	}

	if req.Cwd != "" {
		// client 传来 cwd 时先切过去。失败**不致命**(继续在旧目录执行),但绝不能
		// 静默 —— 原先这里丢掉了退出码,于是"cd 没成功"与"根本没请求切目录"在输出上
		// 完全一样,而使用者以为自己在目标目录里。把它作为一条错误行带回去。
		if _, exit, errs := execSess.Run(ctx, "cd "+shellQuote(req.Cwd), false); exit != 0 {
			note = append(note, errs...)
			note = append(note, fmt.Sprintf("client: 无法切换到 %s,已在原目录 %s 下执行", req.Cwd, execSess.Dir()))
		}
	}

	// stdin 现在是**流式**的:请求头帧之后紧跟裸字节流。
	//
	// 必须在 Run **之前**就接好管道 —— 否则命令先跑起来读不到数据,脚本可能立刻
	// 退出,把后面的字节全丢掉(而且看着是成功的)。
	var stopStdin func()
	switch req.StdinMode {
	case "", ipc.StdinNone:
		// 本次没有 stdin:session 保持空输入(立即 EOF),与真 `bash -c 'cat'` 一致。
	case ipc.StdinStream:
		// 长度模式(StdinLen >= 0)**协议预留、当前未实现**:client 拿不到 os.Stdin
		// 的长度(要读到 EOF 才知道),所以没有任何合法来源会发 >=0 —— 那条 CopyN
		// 分支因此**没有调用方、没有测试**。收到就报错,而不是走一条没人验过的路:
		// 「协议里写了」不能变成「假装支持」,那正是本项目的底线要防的形态。
		// 真要做长度模式(例:文件重定向可直接 f.Stat())时,连同 client 侧发送与
		// 测试一起加,并把 ipc.ProtoVersion 升一位。
		if req.StdinLen != ipc.StdinUntilEOF {
			fail(2, fmt.Sprintf("daemon: 不支持的 stdin_len %d(当前只支持 %d = 读到 EOF)\n"+
				"怎么办: 两端的 winbash.exe 版本可能不一致;运行 `winbash --stop` 后重试",
				req.StdinLen, ipc.StdinUntilEOF))
			return
		}
		s, err := startStdinStream(execSess, conn)
		if err != nil {
			fail(1, "daemon: 接不通 stdin: "+err.Error())
			return
		}
		stopStdin = s
	default:
		// 不认识的 mode 报错,不当成「没有 stdin」—— 那会静默丢掉输入。
		fail(2, fmt.Sprintf("daemon: 不认识的 stdin_mode %q\n"+
			"怎么办: 两端的 winbash.exe 版本可能不一致;运行 `winbash --stop` 后重试", req.StdinMode))
		return
	}

	out, exit, errs := execSess.Run(ctx, req.Script, req.JSON)

	// Run 一返回就回收喂入 goroutine。**顺序不能改**:Run 内部的 defer 已经关掉
	// 读端并换回 /dev/null,但那支 goroutine 可能还卡在 conn.Read 上 ——
	// 关读端/关写端都叫不醒一个正阻塞在网络读上的 goroutine。
	if stopStdin != nil {
		stopStdin()
	}

	_ = writeResponse(conn, ipc.Response{
		OK:     exit == 0,
		Out:    []byte(out),
		Errors: append(note, errs...),
		Exit:   exit,
	})
	closeConnGracefully(conn)
}

// statusText 组装 --status 的输出。
//
// 前三行(pid= / cwd= / addr=)与默认会话的 pwsh= 行必须**保持原样**,client 与测试
// 直接按它们取值;会话池的明细一律追加在后面,且不改动已有行的位置。
func (d *daemon) statusText(conn net.Conn) string {
	d.mu.Lock()
	defer d.mu.Unlock()

	// cwd 用默认会话的目录(它跟着 cd 变)。默认会话还没建起来时(还没有任何一条
	// 命令落到它)报 daemon 进程自己的 cwd —— 不编一个值,也不为了打印去建会话:
	// --status 必须零副作用。
	cwd, pwsh := entryStatusLocked(d.sessions[""], "pwsh=not-started")
	if cwd == "" {
		cwd, _ = os.Getwd()
	}

	var b strings.Builder
	fmt.Fprintf(&b, "pid=%d\n", os.Getpid())
	fmt.Fprintf(&b, "cwd=%s\n", cwd)
	fmt.Fprintf(&b, "addr=%s\n", conn.LocalAddr())
	fmt.Fprintf(&b, "%s\n", pwsh)
	fmt.Fprintf(&b, "sessions=%d running=%d\n", len(d.sessions), d.running)

	// 非默认会话的明细:名字 + pwsh 状态 + 空闲秒数。排序后输出,便于对比与测试。
	names := make([]string, 0, len(d.sessions))
	for name := range d.sessions {
		if name == "" {
			continue // 默认会话已经在上面那几行了
		}
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		e := d.sessions[name]
		_, st := entryStatusLocked(e, "pwsh=not-started")
		fmt.Fprintf(&b, "session=%s %s idle=%ds\n", e.name, st, int(time.Since(e.lastUsed).Seconds()))
	}
	return b.String()
}

// entryStatusLocked 返回一条会话的 (cwd, pwsh 状态行)。调用方必须持 d.mu。
//
// e 为 nil(会话还没建起来)时给空 cwd 与 fallback,由调用方决定兜底值。
// 会话正忙时**读快照**:此时 Session 正被 worker 使用,直接读它的字段是数据竞争
// (shell.Session 明确不并发安全),而 --status 现在是一个并发路径,不再排队。
func entryStatusLocked(e *sessionEntry, fallback string) (dir, pwsh string) {
	if e == nil {
		return "", fallback
	}
	if e.busy {
		return e.snapDir, e.snapPwsh
	}
	// 不忙 = 没有 worker 正在用它,此时读是安全的,而且拿到的是最新值。
	return e.sess.Dir(), e.sess.EngineStatus()
}

// closeConnGracefully 优雅关闭一条连接。
//
// 先发 FIN(CloseWrite),把接收方向留到对端读完响应为止。直接 Close 会让还没读完的
// client 收到 RST —— 实测 `winbash --stop` 因此偶发地报
// "An existing connection was forcibly closed by the remote host"。
// client 通常立刻关闭,所以这里的等待是零成本;真等不到也有 250ms 上限。
//
// 一条连接只该调一次:每条连接的所有权是明确的(要么在 op 路径,要么在 job 路径,
// 两者互斥 —— job 路径的连接一投递就交给 worker 了)。
func closeConnGracefully(conn net.Conn) {
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.CloseWrite()
		_ = conn.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
		_, _ = io.Copy(io.Discard, conn)
	}
	conn.Close()
}

// startStdinStream 把连接上的 stdin 字节流接到 session 的管道上,返回 stop。
//
// 并发结构(单条连接内):建管道 → 起 goroutine 边读连接边写管道 → **立刻返回**,
// 让调用方去跑 Run。必须并发,不能「先读完 stdin 再 Run」—— 那会结构性死锁:
// client 在等响应、不会关连接,而 io.Copy 读不到 EOF 就永远不往下走;且一旦数据
// 超过 os.Pipe 的内核 buffer(64KB),写端会阻塞在一个没有读者的管道上。
//
// stop 在 Run 返回之后被调用,做三件事:设 deadline 唤醒可能卡在 conn.Read 的
// goroutine → 等它真的退出 → 清掉 deadline(免得影响随后的写响应与下一个请求)。
//
// 只读到对端 FIN 为止(StdinLen=StdinUntilEOF):按长度读的形态**没有实现**,
// 校验在 runJob 里,理由见那里。
func startStdinStream(sess *shell.Session, conn net.Conn) (stop func(), err error) {
	pw, err := sess.StdinPipe()
	if err != nil {
		return nil, err
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(pw, conn)
		// 必须关写端:脚本在等 EOF,不关它 Read 就永不返回。
		//
		// 拿到的错误**不上报**。这里最常见的两种都不是故障:
		//   - io.Copy 读到 conn 的 EOF:正常收尾。
		//   - 读端已被 closeStdin 关掉 → Windows 下写返回 Errno(232)
		//     (ERROR_NO_DATA, "The pipe is being closed"),这是「读端已关」的
		//     正常信号,Go 标准库自己也把它当可忽略(os/pipe_test.go 硬断言;
		//     exec_windows.go 同样豁免 232)。报上去就是给每条不读 stdin 的命令
		//     喷一行假错。
		_ = pw.Close()
	}()
	return func() {
		// 唤醒可能阻塞在 conn.Read 上的 io.Copy。关读端/写端都叫不醒网络读,
		// 只有 deadline 或关连接能 —— 这里选 deadline,因为连接随后还要写响应。
		_ = conn.SetReadDeadline(time.Now())
		<-done
		_ = conn.SetReadDeadline(time.Time{})
	}, nil
}

// writeBudget 是单次响应写的上限。
//
// 写也要有上限:客户端发完合法请求却不读响应时,daemon 会卡在 write 上直到 TCP
// 超时 —— 以前 handle 是串行的,一条这样的连接就拖住后面所有命令(与 P0-2 同类)。
// 现在它只拖住自己,但代价只有一行,仍然留着。它需要先持有 token(同用户),
// 所以只算加固项。
const writeBudget = 10 * time.Second

func writeResponse(conn net.Conn, resp ipc.Response) error {
	// 无差别盖章:client 识别「连上的是旧 daemon」靠的就是这个字段为 0。
	// 放在唯一出口而不是各个调用点,免得将来新增一条 return 忘了盖。
	resp.Proto = ipc.ProtoVersion
	_ = conn.SetWriteDeadline(time.Now().Add(writeBudget))
	b, err := ipc.EncodeResponse(resp)
	if err != nil {
		return err
	}
	return ipc.WriteMessage(conn, b)
}

// newToken 生成一个 128 位随机 token(hex 编码,32 字符)。
//
// 用 crypto/rand 而不是 math/rand:后者可预测,而 token 是这里唯一的鉴权凭据。
func newToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// tokenOK 用常数时间比较,避免把 token 的长度/前缀经由响应时间泄漏出去。
//
// want 为空时一律拒绝:那种 daemon 等于没有鉴权,宁可全拒也不要悄悄放行。
func tokenOK(got, want string) bool {
	if want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// restrictEndpointFile 把端口文件的权限收到只有当前用户。
//
// 为什么必须做:os.WriteFile 的 0o600 在 Windows 上**不起作用**(Go 只处理只读位),
// 实测文件的 ACL 里带着 `CodexSandboxUsers:(I)(M)` 这类条目 —— 低权限的沙箱进程因此
// 读得到地址与 token,那样修了 token 也等于白修。
//
// 用 icacls 而不是 Go 的 API:设 DACL 要走 golang.org/x/sys/windows,而它现在是
// **间接**依赖,提成直接依赖就破坏「唯一直接依赖 mvdan/sh」这条约束。icacls 是系统
// 自带的,代价是它可能因本地化或权限失败 —— 那种情况只警告、不改成败:
// daemon 照样能用、token 照样在,只是这层加固没生效,而使用者有权知道。
func restrictEndpointFile(endpoint string) {
	user := os.Getenv("USERNAME")
	if user == "" {
		return
	}
	// /inheritance:r 去掉从父目录继承来的 ACE(沙箱组的权限正是这么来的),
	// /grant:r 只给当前用户读写。
	cmd := exec.Command("icacls", endpoint, "/inheritance:r", "/grant:r", user+":(R,W)")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "winbash: 警告: 收紧端口文件权限失败(%v): %s\n",
			err, strings.TrimSpace(string(out)))
	}
}

// shellQuote 把一段文本变成安全的单引号 shell 字面量。
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// removeIfOwned 只在端口文件里**还是自己的地址**时才删除它。
//
// 为什么必须这么谨慎:daemon A 正在退出时,client 可能已经拉起了 daemon B,
// 并把同一个端口文件写成了 B 的地址。A 若照删不误,就会把 B 的文件删掉 ——
// 之后每个 client 都看到「没在运行」,而后台还挂着一个完好的 B,
// 谁也找不到它。实测 `--stop` 之后紧接着一条命令就会撞上这个窗口。
func removeIfOwned(endpoint, mine string) {
	b, err := os.ReadFile(endpoint)
	if err != nil {
		return
	}
	// 端口文件是两行(地址 + token),只比第一行。
	first := strings.TrimSpace(strings.SplitN(strings.TrimSpace(string(b)), "\n", 2)[0])
	if first == mine {
		_ = os.Remove(endpoint)
	}
}
