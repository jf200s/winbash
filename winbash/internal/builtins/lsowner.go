// 这个文件补上 `ls -l` 里 os.FileInfo 给不出来的三列:属主、属组、硬链接数。
//
// 为什么不塞进 ls.go:这三样都是 Win32 侧的问答(进程 token、文件句柄信息),与
// 「目录怎么遍历、条目怎么排序」无关;混在一起会让 ls.go 的注释被平台细节淹掉。
//
// 为什么不从 os.FileInfo.Sys() 里挖:Windows 上它包的是 WIN32_FILE_ATTRIBUTE_DATA,
// 里面既没有 uid/gid 也没有 NumberOfLinks,硬凑只会让调用方以为拿到了真值。

package builtins

import (
	"os"
	"runtime"
	"sync"
	"syscall"
	"unsafe"
)

// msysIDBase 是 MSYS 把 Windows SID 折成 POSIX uid/gid 的固定偏移:0x30000 + RID。
//
// RID 指 SID 的最后一个子权威,不是整个 SID。实测本机与真 Git Bash 的 `id` 逐位一致:
// uid=197609(Test User) ← 0x30000+1001, gid=197121 ← 0x30000+513。
//
// gid 取的是 **token 的主组**(TokenPrimaryGroup),不是「用户所属的某个组」。
// 本机上两者恰好等价,换过主组的账户上就不等价了,而 `ls -l` 跟的是前者。
const msysIDBase = 0x30000

// TOKEN_INFORMATION_CLASS 的取值,只用到这两个。
const (
	tokenUser         = 1 // TokenUser
	tokenPrimaryGroup = 5 // TokenPrimaryGroup
)

// advapi32 用 NewLazyDLL 而不是 MustLoadDLL:短格式 `ls`(不带 -l)一次都用不到这里,
// 不该因为一个 DLL 加载失败就让整个进程起不来;真取不到时退化成 "unknown"/0 即可。
var (
	advapi32                = syscall.NewLazyDLL("advapi32.dll")
	procGetTokenInformation = advapi32.NewProc("GetTokenInformation")
	procLookupAccountSidW   = advapi32.NewProc("LookupAccountSidW")
)

// ownerOnce / ownerAccount / ownerUID / ownerGID 缓存进程级常量。
//
// 用 sync.Once 是因为 `ls -l` 一个 3000 项的目录会问 3000 次,而这四个值在进程生命周期内
// 不变;每个条目都走一遍 OpenCurrentProcessToken + 两次 GetTokenInformation 是纯浪费。
var (
	ownerOnce    sync.Once
	ownerAccount = "unknown"
	ownerUID     int
	ownerGID     int
)

// ownerName 返回本进程用户账户名,如 "Test User";取不到时返回 "unknown"。
//
// 反直觉但已实测确认:**MSYS 的 ls 不读文件自己的属主**。它显示的永远是当前进程
// token 里的用户 —— 对 C:\Windows\notepad.exe(属主是 TrustedInstaller)真 Git Bash
// 照样打 Test User / 197609。所以「逐文件查 ACL」是错的方向:不但每个条目多一次
// GetNamedSecurityInfo + LookupAccountSidW,结果还会与真 bash 不一致。
// 属主/属组是进程级常量,算一次就够 —— 写错方向不会报错,只会慢且显示错人。
//
// 账户名里**带空格**是正常的(Windows 账户名 "Test User")。调用方不要按空格切列:
// 真 bash 连列宽对齐都因此放弃(各列只用一个空格连接),我们对齐列宽反而与真输出对不上。
func ownerName() string {
	ownerOnce.Do(loadOwner)
	return ownerAccount
}

// ownerIDs 返回 MSYS 口径的 uid/gid,如 197609, 197121;取不到时返回 0, 0。
func ownerIDs() (uid, gid int) {
	ownerOnce.Do(loadOwner)
	return ownerUID, ownerGID
}

// linkCount 返回 path 的硬链接数;打不开或问不到时返回 1。
//
// 走 os.Open + GetFileInformationByHandle,而不是 os.Stat:后者在 Windows 上拿不到
// NumberOfLinks,而前者是在同一个句柄上多问一句,不用第二次打开文件。
// 实测与本机真 bash 一致:普通文件 1、目录 1、C:\Windows\notepad.exe 3
// (它被硬链到 WinSxS 下的其它版本目录,真 bash 也显示 3)。
//
// 失败一律返回 1:真 bash 对「拿不到链接数」的条目同样显示 1,而 0 在 `ls -l` 里是
// 个误导性信号(看起来像坏了,其实只是没问到)。
// linkCount 取硬链接数(一次 open)。
//
// 保留这个薄封装是为了别处(以及测试)能单独问链接数;`ls` 自己走 fileFacts,
// 那样一次 open 能同时拿到链接数、是否可执行,不必为同一文件开两遍。
func linkCount(path string) int {
	links, _ := fileFacts(path, false)
	return links
}

// fileFacts 一次 open 同时取**硬链接数**与**文件头**,判断是否可执行形态。
//
// 为什么要合并:两个信息都需要 open 一次文件 —— 链接数要 GetFileInformationByHandle,
// 「可执行」要读前两字节看是不是 MZ / `#!`。分开写就是同一目录里每个文件 open 两遍,
// 而 `ls -l` 本来就已是本层最重的路径(见 lsItem 注释里那段性能取舍)。
//
// 返回值在 open 失败时退化为 (1, false):链接数 1 是"看起来普通"的保守值,
// 而 exec=false 会让权限位少一个 x —— 两个都是"不会静默说错话"的方向。
func fileFacts(path string, isDir bool) (links int, exec bool) {
	links = 1

	// 目录不需要读文件头(天然带 x,且读目录内容做不了)。
	if isDir {
		f, err := os.Open(path)
		if err != nil {
			return 1, true
		}
		defer f.Close()
		var d syscall.ByHandleFileInformation
		if err := syscall.GetFileInformationByHandle(syscall.Handle(f.Fd()), &d); err != nil {
			return 1, true
		}
		return int(d.NumberOfLinks), true
	}

	f, err := os.Open(path)
	if err != nil {
		return 1, false
	}
	// Fd() 会把文件从 runtime poller 摘到阻塞模式。这里只问一次属性、读两字节就关,
	// 不存在并发读,摘走没有副作用。
	defer f.Close()

	var d syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(syscall.Handle(f.Fd()), &d); err == nil {
		links = int(d.NumberOfLinks)
	}

	// 读前两字节仅为判定文件头(MZ / `#!`)。读不到(空文件、权限)就按"不可执行"。
	//
	// 用 ReadAt 而不是 Read:不移动文件偏移,免得与上面的 ByHandle 调用在
	// 同一 handle 上互相影响(虽然当前实现不读内容,以后若有人在这加读取就容易踩)。
	var head [2]byte
	if n, _ := f.ReadAt(head[:], 0); n == 2 {
		exec = looksExecutable(path, false, head[:])
	} else {
		// 短读(空文件/1 字节)时仍可能靠扩展名可执行(`.exe` 那条)。
		exec = looksExecutable(path, false, nil)
	}
	return links, exec
}

// loadOwner 一次性把账户名、uid、gid 全算好,由 sync.Once 保证只跑一次。
//
// 三样走同一个 token:OpenCurrentProcessToken 拿的是本进程自己的 token,
// 全程不需要额外权限(不需要提权,也不需要 SE_DEBUG)。
func loadOwner() {
	tok, err := syscall.OpenCurrentProcessToken()
	if err != nil {
		return // ownerAccount 已是 "unknown",uid/gid 保持 0
	}
	defer tok.Close()

	if sid := tokenSIDCopy(tok, tokenUser); sid != nil {
		if rid, ok := sidRID(sid); ok {
			ownerUID = msysIDBase + int(rid)
		}
		if name := accountName(sid); name != "" {
			ownerAccount = name
		}
	}

	// 主组单独问一次:它跟用户 SID 是两个不同的 SID(本机 RID 513 vs 1001)。
	if sid := tokenSIDCopy(tok, tokenPrimaryGroup); sid != nil {
		if rid, ok := sidRID(sid); ok {
			ownerGID = msysIDBase + int(rid)
		}
	}
}

// sidHeader 是 SID 的内存布局前缀,用来定位 SubAuthority 数组与算总长。
//
// 为什么要自己声明布局:syscall.SID 是 opaque 类型(`type SID uintptr`),没有任何字段,
// 读不到 SubAuthority / SubAuthorityCount,也没办法从它取到 RID。
// 真实布局是 Revision(1) + SubAuthorityCount(1) + IdentifierAuthority(6)
// + SubAuthority(count 个 uint32),所以 SubAuthority 数组从偏移 8 开始。
type sidHeader struct {
	Revision            byte
	SubAuthorityCount   byte
	IdentifierAuthority [6]byte
}

// sidRID 取 SID 的最后一个子权威(相对标识符)。
//
// 必须取**最后一个**:前半段是域/机器 SID 前缀,同一个域里所有账户都一样,拿它当
// uid 会让全机器的账户显示成同一个号;而 MSYS 的 0x30000+RID 用的正是最后这一段。
func sidRID(sid []byte) (uint32, bool) {
	if len(sid) < int(unsafe.Sizeof(sidHeader{})) {
		return 0, false
	}
	n := int(sid[1])
	if n == 0 || len(sid) < 8+4*n {
		return 0, false
	}
	subs := unsafe.Slice((*uint32)(unsafe.Pointer(&sid[8])), n)
	return subs[n-1], true
}

// accountName 把 SID 翻成账户名,如 "Test User";失败返回空串。
//
// 只取名字,不拼域名:LookupAccountSidW 会顺带给出域名(本机是 "F"),而真 bash 的
// `ls -l` 打的就是 `Test User`、没有 `F\` 前缀。拼上去会让这一列多一个反斜杠,
// 与真输出不再逐字节可比 —— 而逐字节可比正是这一层存在的理由。
func accountName(sid []byte) string {
	if len(sid) < int(unsafe.Sizeof(sidHeader{})) {
		return ""
	}
	// 512 是双倍余量:实测账户名远不到 256 个 UTF-16 单元,缓冲给大了没有代价。
	name := make([]uint16, 512)
	dom := make([]uint16, 512)
	nameLen := uint32(len(name))
	domLen := uint32(len(dom))
	var useType uint32

	// lpSystemName 传 0(NULL)= 查本机;仓库里没有跨机查询的需求。
	r, _, _ := procLookupAccountSidW.Call(0, uintptr(unsafe.Pointer(&sid[0])),
		uintptr(unsafe.Pointer(&name[0])), uintptr(unsafe.Pointer(&nameLen)),
		uintptr(unsafe.Pointer(&dom[0])), uintptr(unsafe.Pointer(&domLen)),
		uintptr(unsafe.Pointer(&useType)))
	if r == 0 {
		return ""
	}

	// nameLen 回来的是**不含**结尾 NUL 的字符数,正好可以直接切;
	// 用整个 512 去转也行,但那是在依赖"缓冲区尾部恰好是 0"。

	// KeepAlive 不是保险起见,而是补上语言层的一个洞:
	// 上面把 &sid[0] / &dom[0] 转成了 uintptr 传给 LazyProc.Call,而 uintptr
	// **不被 GC 当指针追踪**,`//go:uintptrescapes` 只加在最内层 SyscallN 上、
	// 传不到这里。所以从编译器视角看 sid/dom 在这次调用期间"已死"。
	// 实测很难触发(指针槽还在栈帧里,GC 会扫到),但它属于规范外行为 ——
	// 加这两行是零成本的确定性。
	runtime.KeepAlive(sid)
	runtime.KeepAlive(dom)
	return syscall.UTF16ToString(name[:nameLen])
}

// tokenSIDCopy 取出 token 里指定类别的 SID,并**拷贝成独立切片**返回;失败返回 nil。
//
// 为什么必须拷贝(这里踩过一次,别再改回返回指针):
// GetTokenInformation 填出来的 TOKEN_USER / TOKEN_PRIMARY_GROUP 是
// 「SID_AND_ATTRIBUTES 头 + 紧跟其后的 SID 本体」的**一整块**缓冲区,实测 PSID 指向
// 缓冲区内部偏移 0x10。若把那个指针直接返回给调用方,局部 buf 在函数返回后即不可达,
// GC 一旦回收它,指针就悬空 —— 而悬空读在测起来"多半还能跑"(那块内存通常还没被复用),
// 于是这类 bug 会一直藏着,直到某个内存压力大的场合才变成无法归因的错误。
//
// 必须按 Win32 的两次调用写:第一次故意传 nil/0,调用会"失败"
// (ERROR_INSUFFICIENT_BUFFER)并顺手把所需长度写进 n;第二次才真取。
// 只写一次、自己猜个 2048 字节缓冲区的写法看着更短,但它把正确性押在"结构不会变长"上。
func tokenSIDCopy(tok syscall.Token, class uint32) []byte {
	var n uint32
	// 第一次的返回值必然为 0(缓冲区不够),没有意义 —— 要的只是它写出的 n。
	_, _, _ = procGetTokenInformation.Call(
		uintptr(tok), uintptr(class), 0, 0, uintptr(unsafe.Pointer(&n)))
	if n == 0 {
		return nil
	}

	buf := make([]byte, n)
	r, _, _ := procGetTokenInformation.Call(uintptr(tok), uintptr(class),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(n), uintptr(unsafe.Pointer(&n)))
	if r == 0 {
		return nil
	}

	// 结构首字段就是 PSID(对这两个类别都成立)。
	sidPtr := *(*unsafe.Pointer)(unsafe.Pointer(&buf[0]))
	if sidPtr == nil {
		return nil
	}
	// 先借指针读出 SID 头,算出长度,再整块拷贝出来自己持有。
	head := (*sidHeader)(sidPtr)
	size := int(unsafe.Sizeof(sidHeader{})) + 4*int(head.SubAuthorityCount)
	// 上界校验:PSID 指向**这块缓冲区内部**(实测 TokenUser 在偏移 0x10),
	// 所以「偏移 + SID 长度」必须落在 n 以内。
	//
	// 这里**不能**用一个固定的"结构头大小"来算:TokenUser 是 SID_AND_ATTRIBUTES
	// (PSID + DWORD + 对齐 = 16 字节头),而 TokenPrimaryGroup 只有一个 PSID(8 字节头)。
	// 我第一版把 16 硬编码给两个 class 用,结果 gid 直接变成 0 —— 因为对
	// primaryGroup 而言 28+16=44 > 实际 n=36,校验误判越界并返回 nil。
	// 用实际偏移就不依赖任何结构布局,两个 class 一起成立。
	base := uintptr(unsafe.Pointer(&buf[0]))
	off := uintptr(sidPtr) - base
	if size <= 0 || off >= uintptr(n) || off+uintptr(size) > uintptr(n) {
		return nil
	}
	out := make([]byte, size)
	copy(out, unsafe.Slice((*byte)(sidPtr), size))
	return out
}
