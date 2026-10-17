package shell

// isPEImage 是"能不能直接 CreateProcess"的判据,替代了原先那条"扩展名为空"。
// 它错得很具体:所有 `.sh` / `.py` 脚本(有扩展名但不是 PE)都执行不了,而判据本该
// 是"是不是 PE",不是"有没有扩展名"。

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsPEImage(t *testing.T) {
	usr := requireGitUsrBin(t)

	// 真 PE:Git Bash usr\bin 里随便一个 exe。
	for _, name := range []string{"sed.exe", "awk.exe"} {
		p := filepath.Join(usr, name)
		if _, err := os.Stat(p); err != nil {
			continue
		}
		if !isPEImage(p) {
			t.Errorf("%s 是 PE,应判为 true", p)
		}
	}

	// 非 PE:一个带 shebang 的脚本。
	dir := t.TempDir()
	script := filepath.Join(dir, "x.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if isPEImage(script) {
		t.Errorf("%s 是脚本,不是 PE,应判为 false", script)
	}

	// 各种读不出来的情况都按 false(不能因此崩掉)。
	if isPEImage(filepath.Join(dir, "nope.exe")) {
		t.Error("不存在的文件应判为 false")
	}
	if isPEImage(dir) {
		t.Error("目录应判为 false")
	}
	if isPEImage(filepath.Join(dir, "empty.bin")) {
		t.Error("不存在的空文件应判为 false")
	}
	empty := filepath.Join(dir, "empty2.bin")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if isPEImage(empty) {
		t.Error("空文件应判为 false")
	}
}
