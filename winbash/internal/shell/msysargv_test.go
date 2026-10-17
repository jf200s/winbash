package shell

// 这一组测试钉住「③ 层目标是 MSYS 程序时的 argv 编码」。
//
// 起因是一份用户实测反馈:awk / sed 里写双引号会被吃成反斜杠 ——
//
//	awk 'NR==1{print "quoted-ok"}' file  →  awk: NR==1{print \quoted-ok\}
//	                                          "backslash not last character on line"
//
// 这个缺口只在 MSYS/Cygwin 系程序上出现,原生的 node、git 完全正常,所以它既容易被
// 判成「awk 坏了」,也容易被一句「引号写错了」带过去。根因与编码规则见 msysargv.go。

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// 端到端的几条都要真 MSYS 程序;没有 Git Bash 就跳过,不让测试在裸机上假红。
func requireGitUsrBin(t *testing.T) string {
	t.Helper()
	usr := gitBashUsrBin()
	if usr == "" {
		t.Skip("找不到 Git Bash 的 usr\\bin,跳过 MSYS argv 端到端")
	}
	return usr
}

// TestQuoteMSYSArg 是纯函数表:不依赖 Git Bash,钉住编码形状。
//
// 重点不是"好看",而是覆盖那几处**看似能转义、实际不能**的地方:
//   - ' 只能靠切换成双引号段来表达
//   - " 在单引号段里是字面,不需要(也不能)写成 \"
//   - \ 在 MSYS 里没有转义含义,原样留在单引号段里
func TestQuoteMSYSArg(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", `''`},
		{`abc`, `'abc'`},
		{`a b`, `'a b'`},
		// 双引号:单引号段里就是字面,绝不能出现反斜杠
		{`a"b`, `'a"b'`},
		{`"only"`, `'"only"'`},
		// 单引号:切到双引号段,该段一直延伸到遇到 " 才断(所以 's 会一起进去)
		{`it's`, `'it'"'s"`},
		{`'`, `"'"`},
		{`''`, `"''"`},
		// 两者都有:靠相邻段拼接 —— 'a' + "'b" + '"c' 拼回 a'b"c
		{`a'b"c`, `'a'"'b"'"c'`},
		{`'"`, `"'"'"'`},
		// 反斜杠不是转义符,原样留在单引号段里
		{`back\slash`, `'back\slash'`},
		{`\`, `'\'`},
		{`a\"b`, `'a\"b'`},
		// tab / 换行都在引号里原样保留
		{"tab\there", "'tab\there'"},
		{"l1\nl2", "'l1\nl2'"},
		// glob 字符交给 MSYS=noglob 处理,这里只负责原样送达
		{`*?[abc]`, `'*?[abc]'`},
		{`$HOME ~ #!`, `'$HOME ~ #!'`},
		{`中文 "引号"`, `'中文 "引号"'`},
	}
	for _, c := range cases {
		if got := quoteMSYSArg(c.in); got != c.want {
			t.Errorf("quoteMSYSArg(%q) = %q, 期望 %q", c.in, got, c.want)
		}
	}
}

// TestIsMSYSBinary 钉住判定本身 —— 判错了会往原生程序身上套 MSYS 引号,那是反向破坏。
func TestIsMSYSBinary(t *testing.T) {
	usr := requireGitUsrBin(t)
	for _, name := range []string{"awk.exe", "sed.exe", "find.exe"} {
		p := filepath.Join(usr, name)
		if _, err := os.Stat(p); err != nil {
			continue
		}
		if !isMSYSBinary(p) {
			t.Errorf("%s 应判为 MSYS 程序", p)
		}
	}

	// 反例:Windows 自带程序绝不能判成 MSYS。
	if p := filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe"); p != "" {
		if _, err := os.Stat(p); err == nil && isMSYSBinary(p) {
			t.Errorf("%s 不该判为 MSYS 程序", p)
		}
	}

	// 读不出来的路径按 false 处理(保持现状,不乱改原生目标的编码)。
	if isMSYSBinary(filepath.Join(t.TempDir(), "does-not-exist.exe")) {
		t.Error("不存在的文件应判为 false")
	}

	// 自指回归:winbash.exe 自己就带着 "msys-2.0.dll" 这几个字面量(判定表在它里面),
	// 早先那版「全文搜字节串」的判据因此把它自己判成 MSYS 程序,嵌套调用
	// (`winbash -c ...` 走 ③ 层)当场把参数拼坏 —— 冒烟里的「嵌套也拿到管道内容」
	// 就是这么红的。测试二进制同理,它链接了同样的常量。
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if isMSYSBinary(self) {
		t.Errorf("测试二进制自身(%s)导入表里没有 MSYS runtime,不该判为 MSYS", self)
	}
}

// TestMSYSArgvRoundTrip 让真的 MSYS 程序把收到的 argv 原样打回来,字符级比对。
//
// 这是本组测试的核心:编码器对不对,只有 MSYS 的 build_argv 说了算 —— 我们自己
// 怎么推演都不算数。
func TestMSYSArgvRoundTrip(t *testing.T) {
	requireGitUsrBin(t)

	// awk 的 ARGV 就是它解析完收到的参数,拿它当回显器最直接。
	const argvEcho = `awk 'BEGIN{for(i=1;i<ARGC;i++) printf "[%s]", ARGV[i]}'`

	cases := []struct{ name, script, want string }{
		{"无特殊字符", argvEcho + ` abc`, `[abc]`},
		{"双引号", argvEcho + ` 'a"b'`, `[a"b]`},
		{"用户报的 awk 形态", argvEcho + ` 'NR==1{print "quoted-ok"}'`, `[NR==1{print "quoted-ok"}]`},
		{"用户报的 sed 形态", argvEcho + ` 's/q/"x y"/'`, `[s/q/"x y"/]`},
		{"反斜杠", argvEcho + ` 'back\slash'`, `[back\slash]`},
		{"反斜杠加双引号", argvEcho + ` 'a\"b'`, `[a\"b]`},
		{"单引号", argvEcho + ` "it's"`, `[it's]`},
		{"单双引号都有", argvEcho + ` "a'b\"c"`, `[a'b"c]`},
		{"中文与双引号", argvEcho + ` '中文 "引号"'`, `[中文 "引号"]`},
		{"美元符不被展开", argvEcho + ` '$HOME'`, `[$HOME]`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := NewSession()
			out, exit, errsOut := s.Run(context.Background(), c.script, false)
			if exit != 0 {
				t.Fatalf("exit=%d stderr=%v", exit, errsOut)
			}
			if got := trimTrailingNewline(out); got != c.want {
				t.Errorf("argv 被改坏了: got %q, 期望 %q", got, c.want)
			}
		})
	}
}

func trimTrailingNewline(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

// TestMSYSRealProgramsAcceptQuotes 是用户报的两条原始命令本身。
//
// 之所以不跟上面合并:上面证明的是「argv 送达正确」,这里证明的是「awk / sed 真的
// 因此能干活」—— 万一将来有人把 runExternal 改成别的方式送达,这两条会直接报错。
func TestMSYSRealProgramsAcceptQuotes(t *testing.T) {
	requireGitUsrBin(t)

	s := NewSession()
	out, exit, errsOut := s.Run(context.Background(),
		`printf 'first line\nsecond line\n' | awk 'NR==1{print "quoted-ok"}'`, false)
	if exit != 0 {
		t.Fatalf("awk exit=%d stderr=%v", exit, errsOut)
	}
	if got, want := trimTrailingNewline(out), "quoted-ok"; got != want {
		t.Errorf("awk got %q, 期望 %q", got, want)
	}

	out, exit, errsOut = s.Run(context.Background(), `printf 'q\n' | sed 's/q/"x y"/'`, false)
	if exit != 0 {
		t.Fatalf("sed exit=%d stderr=%v", exit, errsOut)
	}
	if got, want := trimTrailingNewline(out), `"x y"`; got != want {
		t.Errorf("sed got %q, 期望 %q", got, want)
	}
}

// TestNativeExeKeepsGoEncoding 是反向保护:原生 exe 必须继续走 Go 默认编码。
//
// 如果有人把 isMSYSBinary 的判定放宽到"全部目标都用 MSYS 编码",这条会红 ——
// 那时 node 会收到带单引号的字面串。
func TestNativeExeKeepsGoEncoding(t *testing.T) {
	usr := requireGitUsrBin(t)
	if _, err := os.Stat(filepath.Join(usr, "awk.exe")); err != nil {
		t.Skip("没有 awk.exe")
	}

	// git.exe 在 mingw64\bin,是原生程序:它不受 MSYS 编码影响。
	gitExe := filepath.Join(filepath.Dir(usr), "..", "mingw64", "bin", "git.exe")
	if _, err := os.Stat(gitExe); err != nil {
		t.Skip("没有 mingw64\\bin\\git.exe")
	}
	if isMSYSBinary(gitExe) {
		t.Error("mingw64 的 git.exe 是原生程序,不该判成 MSYS")
	}
}
