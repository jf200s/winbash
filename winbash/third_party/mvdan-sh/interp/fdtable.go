// Copyright (c) 2017, Daniel Martí <mvdan@mvdan.cc>
// See LICENSE for licensing information

// fdtable.go 是 winbash 对 mvdan/sh 的补丁:进程内的文件描述符(fd)表。
// 动机、行为与剩余边界见 third_party/mvdan-sh/WINBASH-PATCH.md §5。
//
// 真 bash 里 fd 3+ 是一等公民:`exec 3>&1` 建立、`echo x >&3` 使用、命令级
// 重定向(`echo x 3>&1`)只在该条命令内有效、子 shell 各持一份。上游 mvdan/sh
// 只认 0/1/2,其余一律报 `unsupported redirect fd: N` —— 那是功能缺失,
// 不是措辞问题。
//
// 设计三条:
//
//   - fd 0/1/2 **不进表**,它们就是 Runner.stdin/stdout/stderr 三个槽。
//     winbash 的 StdIO 重设机制(closeStdin 每次 Run 换 stdin)直接换这三个槽,
//     复制一份进表立刻与它脱同步。
//   - 表条目持有**引用计数**的底层资源(打开的文件、管道端)。被 dup、被子
//     shell clone、被命令级快照各计一份引用,归零才 Close —— 否则
//     `exec 4>f; exec 5>&4; exec 4>&-` 会把 fd5 脚下的文件提前关掉,
//     那是 use-after-close,比泄漏更糟。
//   - clone 是 map 与条目的**拷贝**、底层资源共享:与 fork 语义一致(共享
//     文件偏移),而且管道左右两段并发跑,共享 map 就是 data race。

package interp

import (
	"fmt"
	"io"
	"maps"
	"strconv"
	"sync/atomic"

	"mvdan.cc/sh/v3/syntax"
)

// fdRes 是 fd 表条目持有的底层资源(一个被打开的文件或管道端)。
// refs 是当前引用它的「表条目 + 表快照」总数,归零才 Close。
type fdRes struct {
	c    io.Closer
	refs atomic.Int32
}

// newFdRes 记第一份引用:谁打开,谁持有。
func newFdRes(c io.Closer) *fdRes {
	res := &fdRes{c: c}
	res.refs.Store(1)
	return res
}

// retain 记一份新引用(dup、clone、快照)。
func (res *fdRes) retain() {
	if res != nil {
		res.refs.Add(1)
	}
}

// release 放弃一份引用;最后一份被放弃时关闭底层资源。
// 并发安全:管道左右两段可能同时在放,归零判定必须是原子的。
func (res *fdRes) release() {
	if res == nil {
		return
	}
	if res.refs.Add(-1) == 0 {
		_ = res.c.Close()
	}
}

// fdEntry 是 fd 表里 fd>=3 的一项,对应 bash 里一个被打开或 dup 的文件描述符。
// in/out 是该 fd 的读/写目标,方向不适用的一侧为 nil;res 是条目引用着的底层
// 资源,可为 nil —— 如 `exec 3>&1` 只是共享 shell 的 stdout,不拥有任何东西。
//
// 条目永远被**整体替换**(bash 的 dup2/close 语义:先关旧 fd 再装新的),
// 所以 in/out 始终来自同一次打开或同一次 dup。
//
// 注意:条目要做 == 比较(快照/恢复时找差异),因此 in/out 的动态类型必须
// 可比较 —— 这里只放指针与 closedWriter{} 这类可比较值。
type fdEntry struct {
	in  io.Reader
	out io.Writer
	res *fdRes
}

// cloneFdTable 复制一份 fd 表给子 shell(含后台命令、管道段与命令替换)。
// map 与条目都是拷贝,底层资源共享并各记一份引用 —— 子 shell 读写父 shell
// 打开的文件(共享偏移),但改自己的 fd 表不影响父,与 fork 一致。
func cloneFdTable(t map[int]fdEntry) map[int]fdEntry {
	if len(t) == 0 {
		return nil
	}
	t2 := maps.Clone(t)
	for _, e := range t2 {
		e.res.retain()
	}
	return t2
}

// fdEntryGet 读 fd 表条目。
func (r *Runner) fdEntryGet(fd int) (fdEntry, bool) {
	e, ok := r.fdTable[fd]
	return e, ok
}

// fdEntrySet 装入 fd 表条目。e.res 必须自带一份引用(newFdRes 出生即带一份,
// 共享来的先 retain);已在表里的旧条目份额在此归还 —— 与 bash 的「重定向
// 到已打开的 fd 先关旧的」一致。
func (r *Runner) fdEntrySet(fd int, e fdEntry) {
	if r.fdTable == nil {
		r.fdTable = make(map[int]fdEntry)
	}
	if old, ok := r.fdTable[fd]; ok {
		old.res.release()
	}
	r.fdTable[fd] = e
}

// fdEntryClose 关闭并移除 fd 表条目(`N>&-` / `N<&-`)。
// fd 不存在时静默成功:真 bash 对「关闭一个已经关掉的 fd」的重定向不报错
// (`echo x 3>&-` 正常输出 x)。
func (r *Runner) fdEntryClose(fd int) {
	old, ok := r.fdTable[fd]
	if !ok {
		return
	}
	delete(r.fdTable, fd)
	old.res.release()
}

// snapshotFds 拍一份 fd 表快照,配合 restoreFds/keepFds 做命令级 save/restore:
// bash 里 `echo x 3>&1` 的 fd 3 只活在那条命令内。快照也算一份引用,
// 这样命令期间 `3>&-` 删掉的条目能随快照回来(与 bash 的「命令级关闭
// 不持久」一致),而被替换/新增的条目在恢复时归还。
func (r *Runner) snapshotFds() map[int]fdEntry {
	if len(r.fdTable) == 0 {
		return nil
	}
	old := maps.Clone(r.fdTable)
	for _, e := range old {
		e.res.retain()
	}
	return old
}

// restoreFds 把 fd 表恢复成快照。
//
// **引用计数必须对称**:snapshotFds 给快照里每一条都 retain 了一份,所以这里
// 要把**当前表的持有全部**归还(无条件 release),再让快照那份转为新的当前表
// 持有 —— 两者抵消后,每条目恰好回到一份。
//
// 早先这里只 release「与快照不同」的条目,漏了两类:① 未变条目(快照与当前表
// 各持一份,恢复后应只剩一份);② 被 `3>&-` 删掉的条目(只在快照里,那份额也
// 得归还)。结果是引用计数永不归零、文件永不关闭 —— `exec 4>f` 的句柄一路
// 漏到会话销毁,Windows 上连 t.TempDir() 的清理都删不掉那个文件(实测踩到)。
func (r *Runner) restoreFds(old map[int]fdEntry) {
	for _, e := range r.fdTable {
		e.res.release() // 当前表的持有全部归还
	}
	r.fdTable = old // 快照那份转为当前表持有
}

// keepFds 是 restoreFds 的镜像:`exec` 的 keepRedirs 让命令级重定向变成
// 持久的,fd 表保持命令跑完的样子。
//
// 同样地,快照那份引用要**无条件**归还 —— 当前表有自己的持有,快照只是一份
// 临时备份。旧写法只归还「被替换/删除」的,未变条目那份就永远留在计数里。
func (r *Runner) keepFds(old map[int]fdEntry) {
	for _, e := range old {
		e.res.release() // 快照的持有全部归还
	}
}

// closeAllFds 归还本表的全部引用,归零的资源被关闭。
// 子 shell 退出、Runner.Reset 与宿主收尾(Session.Close)都走这里 ——
// fd 表是个 map,`*r = Runner{}` 整体重建只会把它静默丢掉,句柄就漏了。
func (r *Runner) closeAllFds() {
	for fd, e := range r.fdTable {
		delete(r.fdTable, fd)
		e.res.release()
	}
	r.fdTable = nil
}

// CloseExtraFds 关闭 fd 表里 fd 3+ 的全部条目(0/1/2 是 stdio 槽,不动)。
// winbash patch:供宿主在会话销毁时收尾(Session.Close),防止常驻进程
// 累积 `exec 4>f` 这类句柄。幂等,可重复调用。
func (r *Runner) CloseExtraFds() {
	r.closeAllFds()
}

// invalidateStdioFds 防 use-after-close:fd 表里凡是引用了**被换掉的** stdio
// 槽旧值的条目(典型如 `exec 3<&0` 之后 winbash 的 closeStdin 把 stdin 管道
// 换成 /dev/null),把对应方向清空 —— 之后再用就报 `3: Bad file descriptor`,
// 而不是去读一个已被 Close 的 *os.File(`file already closed`)。
//
// 槽值没换的不动:那正是「fd 是 stdout 的 dup、写入跟随当次输出」的正确
// 行为(winbash 的 switchWriter 每次 Run 换 buffer,fd 3 应跟着走)。
func (r *Runner) invalidateStdioFds(oldIn stdinFile, oldOut, oldErr io.Writer) {
	for fd, e := range r.fdTable {
		changed := false
		if e.in != nil && e.in == oldIn && r.stdin != oldIn {
			e.in = nil
			changed = true
		}
		if e.out != nil {
			if (e.out == oldOut && r.stdout != oldOut) || (e.out == oldErr && r.stderr != oldErr) {
				e.out = nil
				changed = true
			}
		}
		if changed {
			r.fdTable[fd] = e
		}
	}
}

// redirFd 返回重定向的目标 fd:rd.N 指定,缺省时输入重定向落 fd 0、输出落 fd 1。
// winbash patch:上游只认 0/1/2,其余报 `unsupported redirect fd: N`;
// 现在 3+ 走 fd 表。
func (r *Runner) redirFd(rd *syntax.Redirect, input bool) (int, error) {
	fd := 1
	if input {
		fd = 0
	}
	if rd.N != nil {
		n, err := strconv.Atoi(rd.N.Value)
		if err != nil || n < 0 {
			// {fd} 动态分配(`exec {fd}>file`)之类的非数字形态:不支持,响亮报错。
			return 0, fmt.Errorf("unsupported redirect fd: %v", rd.N.Value)
		}
		fd = n
	}
	// 方向校验:输出重定向不落 fd 0(`0>file` 把 stdin 重开成只写,不支持,
	// 响亮报错)。输入重定向落 fd 1/2 是**标准流反向重开**
	// (`read 1</dev/null`):真 bash 把 fd 重开成只读、之后写它报
	// Bad file descriptor —— 我们近似成把该槽换成 closedWriter(写报同样
	// 错误),读 fd 1/2 做不到(槽是单向 writer),见 redir 与 README。
	if !input && fd == 0 {
		return 0, fmt.Errorf("unsupported redirect fd: %v", rd.N.Value)
	}
	if fd >= 3 {
		r.noteCmdFd(fd)
	}
	return fd, nil
}

// closeStdioWriteSide 处理「输入重定向到标准流」的反向重开形态
// (`read 1</dev/null`):fd 1/2 变成只读,写它要报 `Bad file descriptor`。
// 输入内容不挂载(我们的 stdio 槽没有读侧),资源由调用方按 closer 收。
func (r *Runner) closeStdioWriteSide(fd int) {
	switch fd {
	case 1:
		r.stdout = closedWriter{}
	case 2:
		r.stderr = closedWriter{}
	}
}

// noteCmdFd 记下本条语句用到的最小 fd 3+:子进程继承不了它们(见 exec),
// 外部命令要响亮报错而不是静默丢重定向。
func (r *Runner) noteCmdFd(fd int) {
	if r.cmdFdRedir == 0 || fd < r.cmdFdRedir {
		r.cmdFdRedir = fd
	}
}

// fdDup 返回 fd 当前的内容,供 `>&N` / `<&N` 复制。fd 0/1/2 映射到 stdio 槽。
// ok=false 表示 fd 未打开或方向不适用 —— 真 bash 给 `N: Bad file descriptor`。
func (r *Runner) fdDup(fd int) (fdEntry, bool) {
	switch fd {
	case 0:
		if r.stdin == nil {
			return fdEntry{}, false
		}
		return fdEntry{in: r.stdin}, true
	case 1:
		return fdEntry{out: r.stdout}, r.stdout != nil
	case 2:
		return fdEntry{out: r.stderr}, r.stderr != nil
	default:
		e, ok := r.fdTable[fd]
		return e, ok
	}
}

// redirSetFd 把重定向目标 fd 装成 e:fd 0/1/2 是 stdio 槽(不持有资源引用),
// >=3 走 fd 表(e.res 自带一份引用)。
func (r *Runner) redirSetFd(fd int, e fdEntry) error {
	switch fd {
	case 0:
		stdin, err := newStdinFile(e.in)
		if err != nil {
			return err
		}
		r.stdin = stdin
		return nil
	case 1:
		r.stdout = e.out
	case 2:
		r.stderr = e.out
	default:
		r.fdEntrySet(fd, e)
	}
	return nil
}

// redirCloseFd 关闭目标 fd(`>&-` / `<&-`)。fd 0/1/2 是 stdio 槽:输入侧
// 置 nil、输出侧换成 closedWriter(写时报 Bad file descriptor,见 §4);
// >=3 从表里移除并归还资源。
func (r *Runner) redirCloseFd(fd int, input bool) {
	switch {
	case fd >= 3:
		r.fdEntryClose(fd)
	case input:
		if fd == 0 {
			r.stdin = nil
		}
	default:
		switch fd {
		case 1:
			r.stdout = closedWriter{}
		case 2:
			r.stderr = closedWriter{}
		}
	}
}
