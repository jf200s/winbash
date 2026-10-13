package builtins

import (
	"os"
	"path/filepath"
	"testing"
)

// Git Bash 的 usr\bin 里有 44 个**无扩展名**的可执行脚本(ldd、bzgrep、c_rehash …),
// 它们靠 shebang 而不是扩展名说明自己是什么。
//
// 原来的 Which 只认 PATHEXT 列出的扩展名,于是 `ldd` 直接被判成"找不到"(exit 127),
// 而真 bash 里它好好的。判据改成"头两个字节是 #!"——比"在 PATH 里就当命令"稳:
// PATH 目录里偶尔躺着的 README 之类不会被误认。
func TestWhichFindsExtensionlessShebangScript(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "probe-script")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, ok := Which("probe-script", dir)
	if !ok {
		t.Fatal("无扩展名的 shebang 脚本应当被找到")
	}
	if got != script {
		t.Fatalf("got %q want %q", got, script)
	}
}

// 反面:没有 shebang 的普通文件依然不算命令(否则 PATH 里的说明性文件会被误认)。
func TestWhichIgnoresExtensionlessFileWithoutShebang(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "NOTES"), []byte("just some notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := Which("NOTES", dir); ok {
		t.Fatal("没有 shebang 的普通文件不该被当成命令")
	}
}

// 空文件、只有一个字节的文件都要安全地判成"不是脚本"(不 panic、不误判)。
func TestWhichHandlesTinyFilesSafely(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "empty"), nil, 0o755)
	os.WriteFile(filepath.Join(dir, "onebyte"), []byte("#"), 0o755)

	for _, name := range []string{"empty", "onebyte"} {
		if _, ok := Which(name, dir); ok {
			t.Errorf("%s 不该被当成命令", name)
		}
	}
}

// 扩展名优先级不能被这条改动打乱:同名时仍然取 PATHEXT 命中的那个。
func TestWhichPrefersPathextOverBareScript(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "probe-tool"), []byte("#!/bin/sh\necho bare\n"), 0o755)
	exe := filepath.Join(dir, "probe-tool.exe")
	os.WriteFile(exe, []byte("MZ"), 0o755)

	got, ok := Which("probe-tool", dir)
	if !ok {
		t.Fatal("expected a hit")
	}
	if got != exe {
		t.Fatalf("有 .exe 时应当取 .exe: got %q", got)
	}
}
