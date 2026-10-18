// 这个文件补上会话池的**状态机与关停超时**那几条分支。
//
// 与 stdin_cwd_test.go 的分工:那里测「请求头之后」的执行路径,这里测「请求之间的
// 记账」—— 会话怎么建、队列怎么拒、关停时怎么等 worker。
//
// 为什么这些用**白盒**(直接构造 daemon / sessionEntry)而不是端到端:
//   - 它们的分支大多取决于内部状态(busy / stopped / stopping / 队列满),端到端要
//     构造出那些状态既慢又不可靠;
//   - 但语义本身是确定的纯逻辑,白盒能把断言钉死在"哪个分支给出了什么"。
//
// 刻意**不测**的两类(写了就是不稳定测试,宁可不写):
//   1. pickSession 里"锁外建 Session 期间 daemon 开始关停"与"别人抢先建了同名"两处
//      竞态窗口 —— 它们只在极窄的调度窗口里命中,靠 sleep 碰运气必然 flaky,而注入
//      hook 会把生产代码为测试改造。这两条留作"靠 code review 保证"。
//   2. newToken 的 rand.Read 失败分支 —— 需要注入随机源,收益不抵成本。

package daemon

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"winbash/internal/shell"
)

// newTestDaemon 造一个只够测记账逻辑的 daemon(sessions 必须是非 nil 的 map)。
//
// shutdownDone / stopCtx / stopStop 也一并建好 —— 少了它们,任何走到 shutdown 的
// 代码路径都会以 `close of nil channel` 或 `nil func` 崩掉,而那种崩溃会**掩盖**
// 测试本该给出的诊断信息(实测踩过:watchdog 的变异验证一开始只看到 panic,
// 看不出是哪条断言该红)。
func newTestDaemon() *daemon {
	stopCtx, stopStop := context.WithCancel(context.Background())
	return &daemon{
		sessions:     map[string]*sessionEntry{},
		done:         make(chan struct{}),
		shutdownDone: make(chan struct{}),
		stopCtx:      stopCtx,
		stopStop:     stopStop,
	}
}

// newTestEntry 造一条可用的 sessionEntry。
//
// workerDone 可以自己传:测"worker 卡住"时传一个永不关闭的 channel,
// 测"worker 正常退出"时传一个已关闭的。
func newTestEntry(t *testing.T, name string, workerDone chan struct{}) *sessionEntry {
	t.Helper()
	sess := shell.NewSession()
	t.Cleanup(func() { _ = sess.Close() })
	if workerDone == nil {
		workerDone = make(chan struct{})
	}
	return &sessionEntry{
		name:       name,
		sess:       sess,
		jobs:       make(chan job, 64),
		lastUsed:   time.Now(),
		workerDone: workerDone,
		snapDir:    sess.Dir(),
		snapPwsh:   sess.EngineStatus(),
	}
}

// ★ 会话正忙时,状态查询必须读**快照**,不能读 Session 的实时字段。
//
// 这条是本文件最重要的一个:shell.Session 明确不并发安全,而 `--status` 是并发路径
// (它不再排队,见 statusText 的注释)。busy 时直接读 Dir()/EngineStatus() 就是数据竞争。
//
// 端到端实测证据(手工确认过,写进注释免得下一个人以为是推测):
//
//	job#1 `cd /tmp && pwd`        → 结束后 snapDir = TEMP 目录
//	job#2 `cd /c/Windows && sleep 6` 运行中,busy=true
//	  期间 `winbash --status` 给的 cwd = **TEMP**(job#1 的快照)
//	  而**不是** C:\Windows(job#2 的实时值)
//	job#2 结束后再查 → cwd = C:\Windows(快照此时才更新)
//
// 也就是说"忙时给上一次的快照"是可观测的真实语义,不只是实现细节。
//
// 白盒这里要钉的是分支选择:busy=true 时**必须**返回 snapDir/snapPwsh,
// 哪怕 sess 的实时 Dir() 是另一个值。
func TestEntryStatusLockedPrefersSnapshotWhenBusy(t *testing.T) {
	// 1) e 为 nil:会话还没建起来,给空 cwd 与调用方指定的 fallback。
	if dir, pwsh := entryStatusLocked(nil, "FALLBACK"); dir != "" || pwsh != "FALLBACK" {
		t.Fatalf("nil 会话应当给 (\"\", fallback),得到 (%q, %q)", dir, pwsh)
	}

	e := newTestEntry(t, "busy-session", nil)

	// 2) 不忙:返回**实时**值(此刻没有 worker 在用它,读是安全的)。
	realDir := e.sess.Dir()
	dir, _ := entryStatusLocked(e, "FALLBACK")
	if dir != realDir {
		t.Fatalf("不忙时应当读实时 Dir()=%q,得到 %q", realDir, dir)
	}

	// 3) 忙:必须返回快照。故意把快照设成与实时**明显不同**的值,
	//    这样"读了实时"会立刻被断言抓住 —— 若两者同值,测试就变成假绿。
	e.busy = true
	e.snapDir = `Z:\snapshot-only\dir`
	e.snapPwsh = "pwsh=SNAPSHOT"
	dir, pwsh := entryStatusLocked(e, "FALLBACK")
	if dir != `Z:\snapshot-only\dir` {
		t.Fatalf("busy 时必须读快照,得到 %q —— 读了实时值就是数据竞争"+
			"(shell.Session 不并发安全)", dir)
	}
	if pwsh != "pwsh=SNAPSHOT" {
		t.Fatalf("busy 时 pwsh 也必须读快照,得到 %q", pwsh)
	}
}

// ★ worker 没在预算内退出时,waitWorker 必须**放弃等待**并返回 false。
//
// closeSessions 靠这个返回值决定"跳过主动关闭、让会话随进程回收"。若它退化成
// "一直等到 worker 退出",一次卡死的 worker 就会让 `--stop` 永久挂住 ——
// 而 `--stop` 是版本不匹配时唯一的处置(见 README),挂住等于把唯一出口也堵了。
//
// 用永不关闭的 channel 命中这条分支:确定性,不靠 sleep 碰运气。
func TestWaitWorkerGivesUpAtDeadline(t *testing.T) {
	d := newTestDaemon()
	never := make(chan struct{}) // 永不关闭 = worker 永不退出
	e := newTestEntry(t, "stuck", never)

	deadline := time.Now().Add(50 * time.Millisecond)
	start := time.Now()
	got := d.waitWorker(e, deadline)
	elapsed := time.Since(start)

	if got {
		t.Fatal("worker 从未退出,waitWorker 却返回 true")
	}
	// 必须真的"等到预算用完"而不是立刻放弃:早退会让正常收尾的 worker 被误判卡住。
	if elapsed < 40*time.Millisecond {
		t.Fatalf("只等了 %v 就放弃,应该在 50ms 预算附近才放弃", elapsed)
	}
	// 也不能远远超出预算(它是 10ms 粒度的轮询,给一倍余量)。
	if elapsed > 500*time.Millisecond {
		t.Fatalf("等了 %v,远超 50ms 预算 —— 预算没有生效", elapsed)
	}
}

// ★ worker 正常退出时必须在预算内立刻返回 true(否则 closeSessions 会白跳过关闭)。
func TestWaitWorkerReturnsTrueWhenWorkerDone(t *testing.T) {
	d := newTestDaemon()
	done := make(chan struct{})
	close(done)
	e := newTestEntry(t, "finished", done)

	start := time.Now()
	if !d.waitWorker(e, time.Now().Add(2*time.Second)) {
		t.Fatal("workerDone 已关闭,waitWorker 却返回 false")
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("worker 已退出却等了 %v —— 没有先做一次非阻塞检查", elapsed)
	}
}

// ★ closeSessions 遇到卡住的 worker 必须跳过它、但**不能静默**。
//
// 语义与实测依据:
//  1. 卡住的那条**不**调 e.sess.Close() —— 否则会与正在执行的 Run 撞上,轻则数据
//     竞争、重则 nil 解引用(closeSessions 的注释里记着这个取舍:宁可让 Session
//     随进程消失,也不抢着关)。
//  2. 跳过几条规定要往 stderr 打一行 —— 不静默。否则使用者看到 `--stop` 之后还有
//     进程残留,会当成泄漏 bug 去查。
//
// 断言怎么选(这里返工过一次,记下来):
// 我最初想直接断言"卡住那条没被 Close",但 `Session.Close()` 在没起 ② 层 engine 时
// 是**空操作**(见 shell.Session.Close:`if s.engine == nil { return nil }`),
// 所以"关没关"在当前可观测面上根本不可见 —— 那条断言等于没断言。
// 改成断言**警告里的跳过条数**:它可观测、且两个方向都能抓(错跳过 2 条 → "2 个会话";
// 一条都不跳 → 没有警告)。"卡住那条不被 Close"这一条因此只由 code review 保证。
func TestCloseSessionsSkipsStuckWorkerAndWarns(t *testing.T) {
	d := newTestDaemon()

	// 卡住的:workerDone 永不关闭。
	stuck := newTestEntry(t, "stuck", make(chan struct{}))
	// 正常的:workerDone 已关闭。
	finishedDone := make(chan struct{})
	close(finishedDone)
	finished := newTestEntry(t, "finished", finishedDone)

	// 捕获 stderr:closeSessions 的警告是这条测试的断言对象之一。
	// 用 os.Stderr 的临时重定向,而不是给它加参数 —— 生产签名不该为测试让步。
	oldStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	defer func() { os.Stderr = oldStderr }()

	// 预算给得很小:卡住的那条会耗掉它,正常的那条应当第一次检查就过。
	d.closeSessions([]*sessionEntry{stuck, finished}, 60*time.Millisecond)

	os.Stderr = oldStderr
	_ = w.Close()
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	_ = r.Close()
	warn := buf.String()

	// 1) 必须有一条不静默的警告,并且点名"未在预算内退出"这件事。
	if !strings.Contains(warn, "未在预算内退出") {
		t.Fatalf("跳过了卡住的会话却没有警告 —— 使用者会把进程残留当成泄漏:stderr=%q", warn)
	}
	// 2) **恰好跳过 1 条**:两条里只有 stuck 是不退出的。这条同时钉住两个方向 ——
	//    全都不跳(没有警告)与全都跳(警告说 2 条)都会红。
	if !strings.Contains(warn, "1 个会话") {
		t.Fatalf("应当恰好跳过 1 个会话,警告却说:%q —— "+
			"若说 2 个,说明正常退出的那条也被跳过了", warn)
	}
}

// ★ submit 的三条拒绝分支都必须给出**明确错误**,不能把 job 静默丢掉。
//
// 这三条各自的真实场景:
//   - stopping:   `--stop` 已开始,新请求还在涌入;
//   - e.stopped:  该会话的 worker 已退出(daemon 正在关停);
//   - 队列满:     同一会话里排了 64 条还没跑 —— 这时**拒绝**比无限堆积好,
//     而且必须非阻塞拒绝,否则 worker 回来更新 running 时会与持锁的 submit 死锁。
func TestSubmitRejectsInAllThreeCases(t *testing.T) {
	t.Run("daemon 正在退出", func(t *testing.T) {
		d := newTestDaemon()
		e := newTestEntry(t, "s", nil)
		d.stopping = true
		if err := d.submit(e, job{}); err == nil {
			t.Fatal("stopping 时 submit 必须报错")
		} else if !strings.Contains(err.Error(), "正在退出") {
			t.Fatalf("错误信息没说清原因: %v", err)
		}
	})

	t.Run("会话已停止接收", func(t *testing.T) {
		d := newTestDaemon()
		e := newTestEntry(t, "s", nil)
		e.stopped = true
		if err := d.submit(e, job{}); err == nil {
			t.Fatal("e.stopped 时 submit 必须报错")
		}
	})

	t.Run("队列已满", func(t *testing.T) {
		d := newTestDaemon()
		e := newTestEntry(t, "s", nil)
		// 把队列塞满(容量 64),一个接收者都不起。
		for i := 0; i < cap(e.jobs); i++ {
			if err := d.submit(e, job{}); err != nil {
				t.Fatalf("第 %d 条不该失败: %v", i+1, err)
			}
		}
		if got := d.running; got != cap(e.jobs) {
			t.Fatalf("投递成功 %d 条,running 应当是 %d", cap(e.jobs), got)
		}
		err := d.submit(e, job{})
		if err == nil {
			t.Fatal("队列满了还必须继续接受吗?必须拒绝")
		}
		if !strings.Contains(err.Error(), "队列已满") {
			t.Fatalf("错误信息没说明队列满: %v", err)
		}
		// 拒绝的那条**不该**被计入 running(它从没被接收)。
		if got := d.running; got != cap(e.jobs) {
			t.Fatalf("被拒的 job 不该计入 running: got %d want %d", got, cap(e.jobs))
		}
	})
}

// ★ tokenOK:期望值为空时**一律拒绝**。
//
// 那种 daemon 等于没有鉴权。宁可全拒,也不要在"daemon 忘了生成 token"时悄悄放行 ——
// 而客户端恰好会带空 token,所以 `got=="" && want==""` 这条特别值得钉住:
// 若写成 `got == want`,空对空就是 true,鉴权形同不存在。
func TestTokenOKRejectsEmptyWanted(t *testing.T) {
	if tokenOK("", "") {
		t.Fatal("want 为空时必须拒绝 —— 否则 `got == want` 会让空 token 直接通过")
	}
	if tokenOK("anything", "") {
		t.Fatal("want 为空时,任何 got 都必须拒绝")
	}
	if tokenOK("right", "right") != true {
		t.Fatal("相等的非空 token 应当通过")
	}
	if tokenOK("", "right") {
		t.Fatal("空 got 不该通过")
	}
	if tokenOK("righT", "right") {
		t.Fatal("大小写不同不该通过(这不是模糊匹配)")
	}
	// 前缀不该通过 —— 常数时间比较的语义之一。
	if tokenOK("righ", "right") {
		t.Fatal("前缀不该通过")
	}
}

// ★ 具名会话数到上限时必须**拒绝**,而不是回收最久未用的那一条。
//
// 这是本项目一条明确的取舍(见 pickSession 注释):回收等于**静默销毁状态**,
// 而状态正是"具名会话"存在的理由 —— 一个跑了半天的 `cd` 与变量集合被悄悄换掉,
// 下一次调用看起来成功、实际全在错的前提下跑。宁可让使用者看到一条明确的错误。
//
// 另一个容易写错的点也一并钉住:**默认会话(空名)不占名额**。它不是一个"并行工作流",
// 而是所有不指定会话名的调用共享的那一个,而且它总存在 —— 若计入,使用者会莫名
// 少一个槽(写 16 个名字却只能开 15 个),错误信息里的数字也就对不上。
func TestPickSessionRejectsAtLimitButDefaultDoesNotCount(t *testing.T) {
	d := newTestDaemon()

	// 默认会话先建,并在之后反复取用 —— 它不该影响下面具名会话的计数。
	def, err := d.pickSession("")
	if err != nil {
		t.Fatalf("默认会话不该建不起来: %v", err)
	}
	if !strings.HasPrefix(def.name, "") { // 只是用一下 def,避免 unused
		t.Fatal("unreachable")
	}

	// 建满 maxSessions 个具名会话。
	for i := 0; i < maxSessions; i++ {
		name := "s" + strconv.Itoa(i)
		e, err := d.pickSession(name)
		if err != nil {
			t.Fatalf("第 %d 个具名会话(%s)不该失败: %v", i+1, name, err)
		}
		if e.name != name {
			t.Fatalf("会话名不匹配: got %q want %q", e.name, name)
		}
	}

	// 第 maxSessions+1 个必须被拒绝。
	_, err = d.pickSession("one-too-many")
	if err == nil {
		t.Fatalf("已到上限 %d 却还接受新具名会话", maxSessions)
	}
	if !strings.Contains(err.Error(), "上限") {
		t.Fatalf("错误信息没点明是数量上限: %v", err)
	}
	// 错误里要给出可操作的指路(错误模型的硬要求)。
	// 这里**两条都要**:"--stop 清空"与"复用已有会话名"是两个不同的处置,
	// 少任何一个都让使用者只剩一半选择(只写 --stop 会让人以为必须清空全部会话;
	// 只写复用则不知道资源被谁占着)。
	for _, want := range []string{"怎么办", "--stop", "复用"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误信息缺 %q,指路不完整: %v", want, err)
		}
	}

	// 默认会话在这个状态下**仍然可用** —— 它是零成本兜底,不该被具名会话挤掉。
	if _, err := d.pickSession(""); err != nil {
		t.Fatalf("上限已满时默认会话仍必须可用: %v", err)
	}
	// 已存在的具名会话也仍然可用(上限挡的是"新建",不是"取用")。
	if _, err := d.pickSession("s0"); err != nil {
		t.Fatalf("已存在的具名会话不该因上限被拒: %v", err)
	}

	// 清理:把建出来的会话关掉,免得测试结束时留下 pwsh 之类的余留。
	for _, e := range d.sessions {
		_ = e.sess.Close()
	}
}

// ★ daemon 开始关停后,pickSession 必须**立刻拒绝**,不能还在建会话。
//
// 这是 pickSession 的第一道关(拿锁后先看 stopping)。它的意义:关停流程已经
// 开始摘会话表了,此刻再建一条新会话,那条会话的 worker 没人负责关 —— 泄漏一个
// Session(② 层跑过就会有常驻 pwsh)。
//
// 白盒测这条是合适的:端到端要精确落在"stopping 已置位、但 daemon 还没停止监听"
// 的窗口里,靠 sleep 碰运气必然 flaky。而这里的语义是确定的纯逻辑。
//
// **这条测试的鉴别力边界**(变异验证实测出来,写下来免得下一个人误判):
// 把**第一处** stopping 检查改成 `d.stopping && false`,这条测试仍然绿 —— 因为
// pickSession 后面还有第二处检查(锁外建完 Session 之后那次),它同样会 return
// error 并 Close 掉白建的那份。两者**功能等价**,差别只是多花几微秒建一个 Session
// 再关掉。所以第一处是**性能优化**(避免白建),第二处才是正确性保证。
// 这条测试钉的是那个对外契约(「stopping 时必须报错、且不入表」),两处检查任一
// 存在都能满足 —— 想钉住"第一处必须存在"就得数 Session 分配次数,那是实现细节,
// 不值得为它写测试。把第一处删掉也不会出事,只是白建一份。
func TestPickSessionRejectsWhenStopping(t *testing.T) {
	d := newTestDaemon()
	d.stopping = true

	if _, err := d.pickSession("any"); err == nil {
		t.Fatal("stopping 时 pickSession 必须报错")
	} else if !strings.Contains(err.Error(), "正在退出") {
		t.Fatalf("错误信息没说清原因: %v", err)
	}
	// 默认会话也一样 —— 关停中不该有任何新会话被建出来。
	if _, err := d.pickSession(""); err == nil {
		t.Fatal("stopping 时默认会话也不该被建出来")
	}
	// 一条都不该进表。
	if n := len(d.sessions); n != 0 {
		t.Fatalf("关停中还建出了 %d 条会话", n)
	}
}

// ★ 收紧端口文件权限失败时必须**告警**,不能静默。
//
// restrictEndpointFile 是"token 之外的加固":Windows 上 os.WriteFile 的 0o600 不起作用,
// 实测端口文件的 ACL 里带着沙箱组的 `(M)`,低权限进程因此读得到地址与 token。
// 那段 icacls 若失败,daemon 仍然要继续跑(加固失败不该让服务起不来),但**必须**
// 让使用者知道 —— 否则他会以为端点是私有的。
//
// 用不存在的路径稳定命中失败分支:icacls 对不存在的文件必然报错。
func TestRestrictEndpointFileWarnsWhenIcaclsFails(t *testing.T) {
	if os.Getenv("USERNAME") == "" {
		t.Skip("本机没有 USERNAME,这条分支无从触发")
	}
	// 确认 icacls 真的在(极老的系统或裁剪镜像里可能没有)。
	if _, err := exec.LookPath("icacls"); err != nil {
		t.Skip("本机没有 icacls: " + err.Error())
	}

	oldStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	defer func() { os.Stderr = oldStderr }()

	restrictEndpointFile(`Z:\definitely-no-such-dir-xyz\endpoint.port`)

	os.Stderr = oldStderr
	_ = w.Close()
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	_ = r.Close()
	warn := buf.String()

	if !strings.Contains(warn, "收紧端口文件权限失败") {
		t.Fatalf("icacls 失败却没有任何告警 —— 使用者会以为端口文件是私有的:stderr=%q", warn)
	}
}

// ★ restrictEndpointFile 在拿不到 USERNAME 时必须**什么也不做**而不是瞎调。
//
// 这条分支的真实意义:USERNAME 为空意味着环境异常(服务账户、裁剪过的环境)。
// 此时我们没法构造 icacls 的授权参数,与其传一个 `:(R,W)` 让 icacls 报错刷屏,
// 不如直接返回 —— 权限收紧是加固手段,失败不该让 daemon 起不来。
func TestRestrictEndpointFileNoOpsWithoutUsername(t *testing.T) {
	t.Setenv("USERNAME", "")
	// 传一个不存在的路径:若它没提前返回,icacls 会因路径不存在而报错到 stderr。
	oldStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	defer func() { os.Stderr = oldStderr }()

	restrictEndpointFile(`Z:\definitely-no-such-endpoint-file.port`)

	os.Stderr = oldStderr
	_ = w.Close()
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	_ = r.Close()

	if buf.Len() != 0 {
		t.Fatalf("USERNAME 为空时应当直接返回、不调用 icacls,却输出: %q", buf.String())
	}
}

// ★ restrictEndpointFile 必须授出 DELETE(D),否则 daemon 连自己写的端口文件都删不掉。
//
// 这是「自定义 --endpoint 永久不可用」那条缺陷的源头:只授 (R,W) 时,`--stop`
// 之后文件残留、永远指着已经没人监听的地址,之后每条命令都"连接失败"且永不恢复
// (实测:(R,W) 时 rm 报 Permission denied,改成 (R,W,D) 立刻能删)。
//
// **判据必须是 icacls 的文本,不能用"os.Remove 成功"** —— 在 %TEMP% 下父目录带着
// FILE_DELETE_CHILD,即使文件的 ACL 里没有 D 也删得掉(实测过),那种断言永远绿,
// 测不出这条缺陷。这条测试宁可依赖一个外部命令的输出,也不要一个测不出东西的绿灯。
func TestRestrictEndpointFileGrantsDelete(t *testing.T) {
	user := os.Getenv("USERNAME")
	if user == "" {
		t.Skip("本机没有 USERNAME,无从构造授权参数")
	}
	if _, err := exec.LookPath("icacls"); err != nil {
		t.Skip("本机没有 icacls: " + err.Error())
	}

	ep := filepath.Join(t.TempDir(), "wb.port")
	if err := os.WriteFile(ep, []byte("127.0.0.1:1\ntok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 回归时(只授 R,W)这个文件在 t.TempDir 的清理阶段可能删不掉,那行
	// "TempDir cleanup" 噪音会盖住真正的断言信息。先尽力把 D 补回来再删。
	defer func() {
		_, _ = icaclsRun(ep, "/grant:r", user+":(R,W,D)")
		_ = os.Remove(ep)
	}()

	restrictEndpointFile(ep)

	out, err := icaclsRun(ep)
	if err != nil {
		t.Fatalf("icacls %s 失败: %v: %s", ep, err, out)
	}
	if !aclGrantsDelete(out) {
		t.Fatalf("端口文件的 ACL 里没有 DELETE(D) —— daemon 退出时删不掉自己的文件,"+
			"该 endpoint 会永久不可用。icacls 输出:\n%s", out)
	}
}

// aclGrantsDelete 判断 icacls 的输出里有没有**一项**同时授了 R、W、D。
//
// 不钉字面量 "(R,W,D)":icacls 打印权限项的顺序不由我们决定(可能写成 (D,R,W)),
// 那样会因为一个无关的排版差异假红。判据仍然只有 icacls 的文本这一条路 ——
// 换成"os.Remove 成功"在 %TEMP% 下永远绿(父目录的 FILE_DELETE_CHILD 顶着)。
func aclGrantsDelete(out string) bool {
	for {
		i := strings.IndexByte(out, '(')
		if i < 0 {
			return false
		}
		out = out[i+1:]
		j := strings.IndexByte(out, ')')
		if j < 0 {
			return false
		}
		perms := out[:j]
		out = out[j+1:]

		has := make(map[string]bool, 4)
		for _, p := range strings.Split(perms, ",") {
			has[strings.TrimSpace(p)] = true
		}
		if has["R"] && has["W"] && has["D"] {
			return true
		}
	}
}

// ★ 删除端口文件失败时 removeIfOwned 必须**告警**。
//
// 这条分支是缺陷现场的另一半:文件删不掉时,原先 `_ = os.Remove(...)` 把错误吞掉,
// `--stop` 看起来成功、使用者手上却留着一个指向死地址的文件,下一个调用只得到
// 一句"连接失败",根本不知道该去删哪个文件。本项目不静默做错事 —— 这里要的是
// 一行点名路径与原因的警告。
//
// 怎么构造"可读但删不掉":自己开一个句柄,共享模式**不含 FILE_SHARE_DELETE**。
// 这也是真实世界里最常见的形态 —— 某个程序正开着这个文件。
//
// 为什么不用 icacls deny (D):实测 deny (D) 之后连 os.ReadFile 都会 Access denied,
// 于是 removeIfOwned 走的是「读不到就不删」那条分支,根本到不了删除失败这里。
// 那种构造测到的是另一件事(见 TestRemoveIfOwnedReportsUnreadableEndpoint)。
func TestRemoveIfOwnedReportsDeleteFailure(t *testing.T) {
	dir := t.TempDir()
	ep := filepath.Join(dir, "wb.port")
	addr := "127.0.0.1:60001"
	if err := os.WriteFile(ep, []byte(addr+"\ntok\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	h := holdWithoutDeleteSharing(t, ep)
	defer func() { _ = syscall.CloseHandle(h) }()

	// 前提一:占着的时候仍然读得到(否则测的是别的分支)。
	if _, rerr := os.ReadFile(ep); rerr != nil {
		t.Fatalf("前提不成立:句柄占用时应仍可读,却得到 %v", rerr)
	}
	// 前提二:现在真的删不掉。若删得掉,后面测的根本不是失败分支。
	if rerr := os.Remove(ep); rerr == nil {
		t.Fatal("前提不成立:句柄占用时文件仍能删除,断言会失去意义")
	}

	warn := captureStderr(t, func() { removeIfOwned(ep, addr) })

	// (a) 文件必须还在 —— 删除本来就该失败。
	if _, serr := os.Stat(ep); serr != nil {
		t.Fatalf("文件应当还在(删除本就失败),stat err = %v", serr)
	}
	// (b) 必须有一行警告,并点名路径 —— 只说"删不掉"而不说哪个文件等于没说。
	if !strings.Contains(warn, "删不掉端口文件") || !strings.Contains(warn, ep) {
		t.Fatalf("删除失败却静默(或没点名文件)—— 使用者只看到后来的\"连接失败\",无从查起:"+
			"stderr=%q", warn)
	}
}

// TestRemoveIfOwnedReportsUnreadableEndpoint 钉住"读不到也不能无声"。
//
// 实测:给端口文件 deny (D) 之后 os.ReadFile 会 Access denied。这时
// removeIfOwned 无法判断文件是不是自己的,于是不删 —— 这一步是对的,
// 但静默 return 会让「端口文件读不到」这种真故障完全无声。
func TestRemoveIfOwnedReportsUnreadableEndpoint(t *testing.T) {
	user := os.Getenv("USERNAME")
	if user == "" {
		t.Skip("本机没有 USERNAME,无从构造 deny 参数")
	}
	if _, err := exec.LookPath("icacls"); err != nil {
		t.Skip("本机没有 icacls: " + err.Error())
	}

	dir := t.TempDir()
	ep := filepath.Join(dir, "wb.port")
	if err := os.WriteFile(ep, []byte("127.0.0.1:60001\ntok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 残留的 deny 会让我们连自己建的临时目录都删不掉,所以清理顺序是"先撤再删"。
	defer func() {
		_, _ = icaclsRun(ep, "/remove:d", user)
		_ = os.Remove(ep)
	}()
	if out, derr := icaclsRun(ep, "/deny", user+":(D)"); derr != nil {
		t.Fatalf("icacls /deny (D) 失败: %v: %s", derr, out)
	}
	// 前提:此时确实读不到。若还能读,这条测试就没在测它想测的分支。
	if _, rerr := os.ReadFile(ep); rerr == nil {
		t.Skip("本机 deny (D) 之后仍能读取,构造不出这个分支(不影响其它测试)")
	}

	warn := captureStderr(t, func() { removeIfOwned(ep, "127.0.0.1:60001") })

	if !strings.Contains(warn, "读不到端口文件") || !strings.Contains(warn, ep) {
		t.Fatalf("读不到端口文件却无声 —— 使用者只会看到后续的\"连接失败\":stderr=%q", warn)
	}
}

// TestRemoveIfOwnedStaysSilentWhenMissing 钉住"文件本来就不在"不算故障。
//
// 这是正常路径:client 侧的 forgetStaleEndpoint 可能已经清掉了,或者这次
// --stop 恰好和上一轮清理撞在一起。这时说话只会变成噪音,而噪音会训练使用者
// 忽略警告 —— 那条真正要紧的警告也就废了。
func TestRemoveIfOwnedStaysSilentWhenMissing(t *testing.T) {
	ep := filepath.Join(t.TempDir(), "never-existed.port")

	warn := captureStderr(t, func() { removeIfOwned(ep, "127.0.0.1:60001") })

	if warn != "" {
		t.Fatalf("文件不存在不该警告(它是正常路径),却写了: stderr=%q", warn)
	}
}

// holdWithoutDeleteSharing 打开 path,**不**共享 FILE_SHARE_DELETE。
//
// 效果:文件仍可读,但任何删除都会拿到 sharing violation。用它构造"可读但删不掉"
// 比改 ACL 稳 —— ACL 的 deny (D) 会连带把读也拒掉(实测),那就测错分支了。
func holdWithoutDeleteSharing(t *testing.T, path string) syscall.Handle {
	t.Helper()
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE, nil,
		syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatalf("独占打开 %s 失败: %v", path, err)
	}
	return h
}

// captureStderr 收集 fn 执行期间写到 os.Stderr 的全部内容。
//
// 手法与 TestRestrictEndpointFileWarnsWhenIcaclsFails 一致(临时替换 os.Stderr,
// 不给生产签名加参数)。这里抽出来是因为现在有三条测试都要看警告。
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	defer func() { os.Stderr = old }()

	fn()

	os.Stderr = old
	_ = w.Close()
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	_ = r.Close()
	return buf.String()
}

// icaclsRun 跑一条 icacls 命令并返回它的合并输出。
func icaclsRun(args ...string) (string, error) {
	out, err := exec.Command("icacls", args...).CombinedOutput()
	return string(out), err
}
