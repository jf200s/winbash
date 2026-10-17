// 本文件处理 ③ 层里一类需要**单独一套 argv 编码**的目标:MSYS/Cygwin 系程序。
//
// 为什么不能一套走到底:Windows 上「父进程 → 子进程」的参数是拼成一整条命令行
// 字符串传下去的,由**子进程自己**按它认的规则再拆回 argv。这套规则不是 ABI 里
// 钉死的,各 runtime 各写各的:
//
//	syscall.EscapeArg(Go 默认,MSVC CRT 规则):" 表示字面双引号,\ 是转义符
//	MSYS/Cygwin 的 build_argv(bash 规则):     \ 完全不做转义,引号内外都是字面
//
// 两套规则不兼容,而 Go 只会拼前者。于是凡是从我们(原生进程)启动 MSYS 程序,
// 参数里的双引号就会被 build_argv 解析成「字面 \ + 引号结束」:
//
//	awk 'NR==1{print "quoted-ok"}' file  →  awk 收到 NR==1{print \quoted-ok\}
//	                                       → "backslash not last character on line"
//
// 原生 exe(node、git for Windows)走 MSVC 规则,所以它们一点没受影响 —— 这也解释了
// 为什么这个缺口只在 awk/sed/find/grep 这些 usr\bin 程序上露头。
package shell

import (
	"bytes"
	"encoding/binary"
	"os"
	"strings"
	"sync"
)

// quoteMSYSArg 把单个参数编码成 MSYS build_argv 能原样还原的形式。
//
// 实测出来的 build_argv 规则(16 个用例字符级还原,见 msysargv_test.go):
//
//	1. ' 与 " 都是引号符;引号内的**另一种**引号是字面字符
//	2. \ 在引号内外**都不做转义**,永远按字面收 —— 所以 bash 那套 'a'\''b'
//	   拆分技巧在这里完全无效(build_argv 不认转义序列)
//	3. 相邻的引号段会**拼接**成同一个 argv 元素:'a'"b" 得到 ab
//
// 由 1 与 3 得出唯一完备的编码:按字符选引号 —— 含 ' 的段用 " 包,其余段用 '
// 包,逐段拼接。每个字符只可能落进其中一种段,所以任意字节串都能表达,不需要
// 任何转义,也就绕开了规则 2 那个「没有转义」的死角。
//
// 空串编为 '':bash 里 `cmd ''` 就是一个空参数,build_argv 同样会还原成空串。
func quoteMSYSArg(s string) string {
	if s == "" {
		return "''"
	}
	var b strings.Builder
	// 按字节走而不是按 rune:UTF-8 多字节序列的每个字节都 >= 0x80,
	// 不可能与 '\'' 或 '"' 撞上,所以字节级切分对非 ASCII 是安全的。
	for i := 0; i < len(s); {
		if s[i] == '\'' {
			// 这一段含单引号,只能用双引号包;双引号内的 " 会终止引号,
			// 所以段落必须在 " 处断开(剩下的部分回到下面的单引号分支)。
			j := i
			for j < len(s) && s[j] != '"' {
				j++
			}
			b.WriteByte('"')
			b.WriteString(s[i:j])
			b.WriteByte('"')
			i = j
			continue
		}
		// 其余情况(包括含 " 与 \ 的段)用单引号包:单引号内一切都是字面。
		j := i
		for j < len(s) && s[j] != '\'' {
			j++
		}
		b.WriteByte('\'')
		b.WriteString(s[i:j])
		b.WriteByte('\'')
		i = j
	}
	return b.String()
}

// msysBinaryCache 缓存 isMSYSBinary 的判定:判定要读可执行文件的头部,
// 不能进热路径。键是绝对路径,值是 bool。
//
// 不做过期:同一个进程生命周期内一个可执行文件被原地替换的概率极低,而真发生了
// 也只是这一层判定沿用旧结论,不影响已经跑起来的程序。
var msysBinaryCache sync.Map // string -> bool

// msysRuntimeDLLs 是各代 MSYS/Cygwin runtime 的 DLL 名。命中任意一个就说明
// 这个 exe 的 argv 由 build_argv 解析。
var msysRuntimeDLLs = []string{"msys-2.0.dll", "msys-1.0.dll", "cygwin1.dll"}

// isMSYSBinary 判断 abs 指向的是不是 MSYS/Cygwin 系程序(决定用哪套 argv 编码)。
//
// 判定靠 PE 导入表:导入 msys-2.0.dll / cygwin1.dll 的就是。实测
// usr\bin\awk.exe sed.exe find.exe env.exe bash.exe sort.exe xargs.exe → true,
// mingw64\bin\git.exe、System32\cmd.exe、System32\find.exe → false。
//
// 试过但**不行**的两条路:
//
//	debug/pe 的 ImportedLibraries() —— 它按段名 ".idata" 找导入表,而 Git Bash 的
//	                                  exe 与 cmd.exe 都把导入表放在别的段里,于是
//	                                  它对**所有**文件都返回空表,拿不到任何结论。
//	                                     恒为空的判据比没有判据更危险。
//	在文件里搜 "msys-2.0.dll" 字节串 —— 会**自指**:winbash.exe 里本来就带着这几个
//	                                  字面量,于是它把自己判成 MSYS 程序,嵌套调用
//	                                  (`winbash -c ...` 走 ③ 层)当场把参数拼坏。
//	                                  冒烟脚本的「嵌套也拿到管道内容」就是这么红的。
//
// 读不出来(非 PE 文件、权限不足)按 false 处理 —— 保持现状,即沿用 Go 默认编码。
// 宁可没修好也不要乱改:原生目标的参数若被我们套上 MSYS 引号,node、git 收到的
// 就是带引号的字面串,那是更坏的破坏。
func isMSYSBinary(abs string) bool {
	if v, ok := msysBinaryCache.Load(abs); ok {
		return v.(bool)
	}
	v := importsMSYSRuntime(abs)
	msysBinaryCache.Store(abs, v)
	return v
}

func importsMSYSRuntime(abs string) bool {
	data, err := os.ReadFile(abs)
	if err != nil {
		return false
	}
	for _, dll := range importedDLLs(data) {
		for _, want := range msysRuntimeDLLs {
			if strings.EqualFold(dll, want) {
				return true
			}
		}
	}
	return false
}

// importedDLLs 按 PE 规范走 DataDirectory[1](导入表)读出所有被导入的 DLL 名。
//
// 不依赖段名(那正是 debug/pe 失败的坑),只按 RVA 找数据 —— 导入表在 .idata、
// .rdata 还是别的段里都能读到。不是合法 PE 就返回 nil。
func importedDLLs(data []byte) []string {
	const (
		pe32Magic     = 0x10b
		pe32PlusMagic = 0x20b
		sizeOfSection = 40
	)
	u16 := func(off int) uint32 {
		if off+2 > len(data) {
			return 0
		}
		return uint32(binary.LittleEndian.Uint16(data[off:]))
	}
	u32 := func(off int) uint32 {
		if off+4 > len(data) {
			return 0
		}
		return binary.LittleEndian.Uint32(data[off:])
	}

	if len(data) < 0x40 || data[0] != 'M' || data[1] != 'Z' {
		return nil
	}
	peOff := int(u32(0x3c))
	if peOff+24 > len(data) || !bytes.Equal(data[peOff:peOff+4], []byte("PE\x00\x00")) {
		return nil
	}
	coff := peOff + 4
	numSections := int(u16(coff + 2))
	optSize := int(u16(coff + 16))
	opt := coff + 20
	if optSize < 2 || opt+optSize > len(data) {
		return nil
	}

	// 可选头里 DataDirectory 的偏移随 PE32 / PE32+ 不同。
	var dd int
	switch u16(opt) {
	case pe32Magic:
		dd = opt + 96
	case pe32PlusMagic:
		dd = opt + 112
	default:
		return nil
	}
	if dd+16 > opt+optSize {
		return nil
	}
	// DataDirectory[1] = 导入表(RVA, Size)。
	importRVA := u32(dd + 8)
	if importRVA == 0 {
		return nil
	}

	// RVA → 文件偏移:靠段表把虚拟地址映射回文件里。
	rvaToOffset := func(rva uint32) (int, bool) {
		sections := opt + optSize
		for i := 0; i < numSections; i++ {
			sh := sections + i*sizeOfSection
			if sh+sizeOfSection > len(data) {
				return 0, false
			}
			va := u32(sh + 12)
			rawSize := u32(sh + 16)
			rawPtr := u32(sh + 20)
			if rva >= va && rva < va+rawSize {
				off := int(rva-va) + int(rawPtr)
				if off >= 0 && off < len(data) {
					return off, true
				}
			}
		}
		return 0, false
	}

	// 导入目录是一串 20 字节的描述符,以全零项收尾;每项的 +12 是 DLL 名 RVA。
	impOff, ok := rvaToOffset(importRVA)
	if !ok {
		return nil
	}
	var out []string
	for i := 0; ; i++ {
		d := impOff + i*20
		if d+20 > len(data) {
			break
		}
		nameRVA := u32(d + 12)
		if nameRVA == 0 {
			break
		}
		nameOff, ok := rvaToOffset(nameRVA)
		if !ok {
			break
		}
		end := nameOff
		for end < len(data) && data[end] != 0 {
			end++
		}
		out = append(out, string(data[nameOff:end]))
		if len(out) >= 256 { // 防御:损坏的映像不该让我们无限往下走
			break
		}
	}
	return out
}
