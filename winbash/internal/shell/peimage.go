package shell

import (
	"encoding/binary"
	"io"
	"os"
)

// isPEImage 判断 path 是不是 Windows 的 PE 映像 —— 也就是 CreateProcess 唯一能
// 直接启动的那类文件。
//
// 为什么要看文件头而不是扩展名(这是修掉的一个真缺口):原来的判据是「扩展名为空
// 就走 env.exe」,它两头都错:
//
//	`./t.sh`(有扩展名,但不是 PE)  → 落到 CreateProcess,报 126
//	                                 "%1 is not a valid Win32 application"
//	`c_rehash`(无扩展名,是 Perl)   → 这条路上碰巧对
//
// 也就是说,所有 `.sh` / `.py` 脚本(有扩展名但不是 PE)都执行不了,而真正的判据
// 应该是「它是不是 PE」,不是「它有没有扩展名」。
//
// 读头很便宜(0x40 字节 + e_lfanew 处 4 字节),进热路径也无所谓 —— 它不读整个文件,
// 和 isMSYSBinary 那种扫描导入表的开销不是一个量级。
func isPEImage(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	var dos [0x40]byte
	if _, err := io.ReadFull(f, dos[:]); err != nil {
		return false
	}
	if dos[0] != 'M' || dos[1] != 'Z' {
		return false
	}
	peOff := int64(binary.LittleEndian.Uint32(dos[0x3c:]))
	var sig [4]byte
	if _, err := f.ReadAt(sig[:], peOff); err != nil {
		return false
	}
	return sig[0] == 'P' && sig[1] == 'E' && sig[2] == 0 && sig[3] == 0
}
