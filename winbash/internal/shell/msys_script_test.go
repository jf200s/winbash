package shell

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"winbash/internal/builtins"
)

// MSYS 的**无扩展名脚本**不能直接交给 Windows 的 CreateProcess —— 它只认 PE 文件。
// 这类脚本得由 Git Bash 来解释。`egrep`/`fgrep` 当时是用 ① 层同义词绕过去的,
// 这条测试管的是**通用**那条路(usr\bin 里有 44 个,例如 ldd、bzgrep、c_rehash)。
func TestExtensionlessShebangScriptRunsViaGitBash(t *testing.T) {
	requireGitBash(t)

	dir := t.TempDir()
	script := filepath.Join(dir, "probe-script")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho from-script \"$1\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// 把临时目录挂进 PATH:Session 在 NewSession 时读进程 PATH 构造搜索路径。
	t.Setenv("PATH", dir+string(filepath.ListSeparator)+os.Getenv("PATH"))

	s := NewSession()
	out, exit, errsOut := s.Run(context.Background(), `probe-script hello`, false)
	if exit != 0 {
		t.Fatalf("exit=%d out=%q errs=%v", exit, out, errsOut)
	}
	if got := strings.TrimSpace(out); got != "from-script hello" {
		t.Errorf("got %q want %q(参数要原样传到脚本里)", got, "from-script hello")
	}

	// 退出码也要原样带回来 —— 脚本跑起来了,结果就是它的结果。
	out, exit, _ = s.Run(context.Background(), `probe-script; echo "exit=$?"`, false)
	_ = out
	if exit != 0 {
		t.Errorf("脚本的参数是空的也该正常结束: exit=%d", exit)
	}
}

// 管道与退出码都要按 bash 语义穿过这一层:无扩展名脚本在管链中间同样要工作。
func TestExtensionlessScriptWorksInsidePipeline(t *testing.T) {
	requireGitBash(t)

	dir := t.TempDir()
	script := filepath.Join(dir, "probe-echoer")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ncat\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(filepath.ListSeparator)+os.Getenv("PATH"))

	s := NewSession()
	out, exit, errsOut := s.Run(context.Background(), `printf 'a\nb\n' | probe-echoer | wc -l`, false)
	if exit != 0 {
		t.Fatalf("exit=%d errs=%v", exit, errsOut)
	}
	if strings.TrimSpace(out) != "2" {
		t.Errorf("got %q want 2", strings.TrimSpace(out))
	}
}

// 真环境的那 44 个之一:确认 usr\bin 里的无扩展名脚本能被我们**定位**到。
//
// 这里只验"找到"(执行路径已由上面那两个自造脚本的测试覆盖):
// 拿真实脚本去跑会带副作用 —— bzless 会拉起 pager、addgnupghome 会建目录。
// 也不赌具体名字:`ldd` 就不在这台机器的 usr\bin 里(它只是常被举的例子)。
func TestExtensionlessScriptsInGitBashUsrBinAreFound(t *testing.T) {
	usrBin := gitBashUsrBin()
	if usrBin == "" {
		t.Skip("这台机器上没有标准位置的 Git Bash")
	}

	entries, err := os.ReadDir(usrBin)
	if err != nil {
		t.Skipf("读不到 %s: %v", usrBin, err)
	}

	var name string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != "" {
			continue
		}
		if hasShebang(filepath.Join(usrBin, e.Name())) {
			name = e.Name()
			break
		}
	}
	if name == "" {
		t.Skip("这台机器的 usr\\bin 里没有无扩展名的 shebang 脚本")
	}

	got, ok := builtins.Which(name, usrBin)
	if !ok {
		t.Fatalf("%s 应当被 Which 找到(它就是这台机器上那类脚本)", name)
	}
	if got != filepath.Join(usrBin, name) {
		t.Fatalf("got %q want %q", got, filepath.Join(usrBin, name))
	}
}

// shebang 的解释器由脚本自己说了算 —— 这条钉住"不是硬编码 bash"。
//
// 起因是实测:`c_rehash` 是 **Perl** 脚本,而我第一版用 `bash <script>` 去跑它,
// 于是得到 `line 15: my: command not found`(bash 在解析 Perl 的 `my @dirs`)。
// 现在改成让 MSYS 的 execve 读 shebang,所以 `#!/bin/sh` 与 `#!/usr/bin/env sh`
// 这两种写法都必须能跑通。
func TestShebangIsRespectedNotHardcoded(t *testing.T) {
	requireGitBash(t)

	for _, tc := range []struct{ name, shebang string }{
		{"probe-plain", "#!/bin/sh\n"},
		{"probe-env", "#!/usr/bin/env sh\n"},
	} {
		dir := t.TempDir()
		script := filepath.Join(dir, tc.name)
		body := tc.shebang + "echo \"shebang-ok:$1\"\n"
		if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", dir+string(filepath.ListSeparator)+os.Getenv("PATH"))

		s := NewSession()
		out, exit, errsOut := s.Run(context.Background(), tc.name+" arg1", false)
		if exit != 0 {
			t.Errorf("%s: exit=%d errs=%v", tc.name, exit, errsOut)
			continue
		}
		if got := strings.TrimSpace(out); got != "shebang-ok:arg1" {
			t.Errorf("%s: got %q want %q", tc.name, got, "shebang-ok:arg1")
		}
	}
}

// requireGitBash 在找不到"带 env.exe 的 Git Bash"时跳过 —— ③ 层解释无扩展名脚本
// 靠的正是 MSYS 的 env.exe(它读 shebang 找解释器)。
func requireGitBash(t *testing.T) {
	t.Helper()
	usrBin := gitBashUsrBin()
	if usrBin == "" {
		t.Skip("这台机器上没有标准位置的 Git Bash")
	}
	if st, err := os.Stat(filepath.Join(usrBin, "env.exe")); err != nil || st.IsDir() {
		t.Skipf("%s 里没有 env.exe,③ 层没法解释 shebang 脚本", usrBin)
	}
}

// hasShebang 是 builtins.isScript 在测试侧的最小复刻(那个函数没导出)。
func hasShebang(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	var head [2]byte
	if _, err := f.Read(head[:]); err != nil {
		return false
	}
	return head[0] == '#' && head[1] == '!'
}
