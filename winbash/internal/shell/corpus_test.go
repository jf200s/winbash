package shell

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"winbash/internal/testenv"
)

// 这一组测试把 winbash 的**纯 shell 语义**与真 bash 逐条对照 —— 语料取
// 「AI 常写的写法」,而不只是 pure-bash-bible 的章节(spec §10 第五类)。
//
// 为什么要跑真 bash 而不是写死期望值:写死的是"我以为 bash 会怎样"。
// 这一轮的语料扫描就是靠它抓到 `${v/#前缀/替换}` 静默原样输出、
// `find`/`sort` 被 Windows 同名工具抢走、`command -v ls` 误判为不存在这三类的。
//
// 每条语料用**新的 Session**:winbash 是常驻会话,`set -e`、`trap`、`alias`、
// 变量都会跨命令留存 —— 不隔离的话,一条 `set -e` 会把后面所有结果带偏。
//
// 找不到真 bash 就整组跳过(不是失败):这台机器上 Git Bash 是 AI 的 Unix 参照系,
// 但它不是 winbash 的依赖。
func TestBashCorpusMatchesRealBash(t *testing.T) {
	bash := findRealBash(t)
	if testing.Short() {
		t.Skip("short mode: 跳过与真 bash 的逐条对照")
	}

	for _, c := range bashCorpus {
		c := c
		t.Run(c.name(), func(t *testing.T) {
			// 语料里的临时文件名带 WBTMP 占位符 —— 换成本次测试自己的临时目录。
			// 原先这些用例把文件写进**包源码目录**(wb-corpus-1.txt 之类),一旦
			// 某条用例的 rm 失败就会在仓库里留垃圾,而且诊断信息完全看不到。
			// 替换对两条执行路径一致,不影响可比性。
			script := strings.ReplaceAll(c.script, "WBTMP", filepath.ToSlash(t.TempDir()))

			want, wantExit := runRealBash(t, bash, script)

			s := NewSession()
			got, gotExit, errsOut := s.Run(context.Background(), script, false)
			got = strings.TrimRight(got, " \t\r\n")

			if c.known != "" {
				// 已知且有理由的差异。这里**必须断言差异仍然存在**:
				// 原先只在"两边恰好一致"时 Skip、其余情况直接 return —— 那等于一个
				// 不做任何检查的空壳,一条过期的 known 会永远静静地留在表里,
				// 而 README 却声称"差异全部有测试钉住"。
				//
				// 没有去解析 known 的描述文本(那是自然语言,解析它只会造出脆弱的断言),
				// 但"差异确实还在"这一点,足以让过期的 known 自己浮出来。
				if got == want && gotExit == wantExit {
					t.Errorf("这条 known 差异已经不存在了(两边一致),请把它删掉: %s", c.known)
				}
				return
			}

			if got != want || gotExit != wantExit {
				t.Errorf("与真 bash 不一致\n  script : %s\n  bash   : exit=%d out=%q\n  winbash: exit=%d out=%q errs=%v",
					c.script, wantExit, want, gotExit, got, errsOut)
			}
		})
	}
}

// corpusCase 一条语料。known 非空表示"这是已知且有理由的差异",内容说明差在哪、为什么接受。
type corpusCase struct {
	script string
	known  string
}

func (c corpusCase) name() string {
	n := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		default:
			return '-'
		}
	}, c.script)
	if len(n) > 60 {
		n = n[:60]
	}
	return n
}

var bashCorpus = []corpusCase{
	// ---- 字符串与参数展开 ----
	{script: `v=hello; echo ${v:1:3}`},
	{script: `v=hello; echo ${#v}`},
	{script: `v=a/b/c; echo ${v##*/}`},
	{script: `v=a/b/c; echo ${v%/*}`},
	{script: `v=a/b/c; echo ${v//\//-}`},
	{script: `v=abc; echo ${v^^}`},
	{script: `v=ABC; echo ${v,,}`},
	{script: `f=file.tar.gz; echo ${f%%.*}`},
	{script: `unset v; echo ${v:-def}`},
	{script: `unset v; echo ${v:+alt}`},
	{script: `v=; echo ${v:-empty}`},
	{script: `unset v; echo ${v:=set}; echo $v`},
	{script: `v=abcabc; echo ${v/abc/X}`},
	{script: `v=abcabc; echo ${v//abc/X}`},
	{script: `v=prefix-x; echo "${v#prefix}"`},
	{script: `v=prefix-x; echo "${v%%-x}"`},
	{script: `echo ${undefined_var}`},
	{script: `v='a b'; echo "${v}"`},
	{script: `v='a b'; echo ${v}`},
	{script: `echo $'tab\there'`},
	{script: `echo "quote: \"x\""`},
	{script: `printf '%s=%d\n' x 1`},
	{script: `printf '%s\n' a b c`},
	{script: `printf '%5s|\n' hi`},

	// ---- 数组 ----
	{script: `a=(1 2 3); echo ${a[1]}`},
	{script: `a=(1 2 3); echo ${#a[@]}`},
	{script: `a=(1 2 3); echo ${a[@]:1}`},
	{script: `a=(); a+=(x); echo ${a[@]}`},
	{script: `a=(one two three); for x in "${a[@]}"; do echo "[$x]"; done`},
	{script: `a=(1 2 3); unset 'a[1]'; echo "${a[@]}"`},
	{script: `declare -A m; m[k]=v; echo ${m[k]}`},
	{script: `a=(1 2 3); echo "${a[*]}"`},

	// ---- 算术与条件 ----
	{script: `echo $((2+3*4))`},
	{script: `echo $((10/3))`},
	{script: `echo $((2**10))`},
	{script: `echo $((7%3))`},
	{script: `n=5; (( n > 3 )) && echo yes`},
	{script: `n=5; echo $((n+1))`},
	{script: `(( 0 )) || echo zero-is-false`},
	{script: `[[ -n "x" ]] && echo nonempty`},
	{script: `[[ "abc" == a* ]] && echo match`},
	{script: `[[ "abc" =~ ^a.c$ ]] && echo regex`},
	{script: `[[ 1 -lt 2 ]] && echo lt`},
	{script: `[ -f go.mod ] && echo file`},
	{script: `[ -d . ] && echo dir`},
	{script: `[ "a" = "b" ] || echo ne`},
	{script: `[[ -z "" ]] && echo empty`},

	// ---- 控制流 ----
	{script: `for i in 1 2 3; do echo $i; done`},
	{script: `for ((i=0;i<3;i++)); do echo $i; done`},
	{script: `while read -r l; do echo "L:$l"; done <<< $'a\nb'`},
	{script: `case x in x) echo x;; *) echo other;; esac`},
	{script: `case y in x|Y) echo xy;; y) echo y;; esac`},
	{script: `if [ 1 = 1 ]; then echo eq; fi`},
	{script: `until false; do echo once; break; done`},
	{script: `i=0; while [ $i -lt 2 ]; do echo $i; i=$((i+1)); done`},
	{script: `for f in *.mod; do echo $f; done`},
	// glob 的结果是 Windows 原生形式(`.\classify.go`),bash 给 `./classify.go`。
	// 我们**接受**这个差异:这一层的路径本来就是 Windows 形式,而 ①/②/③ 层
	// 两种写法都能吃;只有"拿 glob 结果跟 POSIX 字面量做字符串比较"时才会不同。
	// (语料用 .go 不用 .mod:这个测试跑在包目录里,.go 才必然存在。)
	{script: `for f in ./*.go; do echo $f; done | head -1`,
		known: "glob 展开是 Windows 原生形式(.\\classify.go),bash 给 ./classify.go"},

	// ---- 命令替换与管道 ----
	{script: `echo "$(echo nested)"`},
	{script: `x=$(printf 'a\nb'); echo "$x"`},
	{script: `x=$(printf 'a\nb'); echo $x`},
	{script: `echo a b c | wc -w`},
	{script: `printf 'a\nb\n' | grep b`},
	{script: `echo hello | tr a-z A-Z`},
	{script: `seq 3 | tail -1`},
	{script: `echo a:b | cut -d: -f2`},
	{script: `echo abc | cut -c1`},
	{script: `echo 'a b' | awk '{print $2}'`},
	{script: `printf 'a\na\nb\n' | uniq -c`},
	{script: `echo a b | xargs -n1 echo`},
	{script: `ls | xargs -n1 echo | wc -l`},
	{script: `echo x | cat | cat`},
	{script: `{ echo a; echo b; } | wc -l`},
	{script: `(echo a; echo b) | wc -l`},
	{script: `echo a; echo b | wc -l`},
	{script: `! false && echo negated`},
	{script: `true && echo yes || echo no`},
	{script: `false || echo fallback`},

	// ---- 重名工具的裁决(本轮修的就是这类:Windows System32 也有 sort/find)----
	{script: `find . -maxdepth 1 -name '*.mod'`},
	{script: `printf 'b\na\n' | sort`},
	{script: `sort --version 2>&1 | head -1`},
	{script: `echo b a | sort | head -1`},
	{script: `find . -maxdepth 1 -name '*.go' | head -1 | wc -l`},

	// ---- 重定向与文件 ----
	{script: `echo hi > "WBTMP/wb-corpus-1.txt"; cat "WBTMP/wb-corpus-1.txt"; rm -f "WBTMP/wb-corpus-1.txt"`},
	{script: `printf 'x\n' | tee "WBTMP/wb-corpus-2.txt" > /dev/null; cat "WBTMP/wb-corpus-2.txt"; rm -f "WBTMP/wb-corpus-2.txt"`},
	{script: `echo hi 2>&1`},
	{script: `echo out; echo err 1>&2`},
	{script: `: > "WBTMP/wb-corpus-3.txt"; wc -c < "WBTMP/wb-corpus-3.txt"; rm -f "WBTMP/wb-corpus-3.txt"`},
	{script: `cat < go.mod | wc -l`},
	{script: `v=$(cat <<EOF
line1
line2
EOF
); echo "$v" | wc -l`},
	{script: `cat <<'EOF' | wc -l
a
b
c
EOF`},

	// ---- 函数、内建、退出码 ----
	{script: `f() { echo "got $1"; }; f x`},
	{script: `f() { local v=1; echo $v; }; f`},
	{script: `f() { echo $#; }; f a b c`},
	{script: `f() { return 3; }; f; echo $?`},
	{script: `x=1; x=$((x+1)); echo $x`},
	{script: `read -r x <<< hi; echo $x`},
	{script: `IFS=, read -r a b <<< "1,2"; echo "$a-$b"`},
	{script: `mapfile -t L <<< $'a\nb'; echo ${#L[@]}`},
	{script: `echo {1..3}`},
	{script: `echo {a..c}`},
	{script: `echo {1..6..2}`},
	{script: `set -e; false; echo unreachable`},
	{script: `true; echo $?`},
	{script: `false; echo $?`},
	{script: `echo -n x; echo`},
	{script: `echo -e "a\tb"`},
	{script: `echo -E "a\tb"`},
	{script: `v=1; unset v; echo "${v-unset-marker}"`},
	{script: `export WB_CORPUS_X=1; echo $WB_CORPUS_X`},
	{script: `alias ll='echo aliased'; ll`},
	{script: `echo "$(pwd | wc -l)"`},

	// ---- 存在性检测(AI 的标准写法;`command -v` 曾经静默误判我们自己的内建)----
	{script: `command -v ls >/dev/null && echo found`},
	{script: `command -v git >/dev/null && echo found`},
	{script: `command -v sed >/dev/null && echo found`},
	{script: `command -v definitely-not-installed-xyz >/dev/null 2>&1 || echo missing`},
	{script: `type echo`},

	// ---- 已知差异:每一条都写清楚差在哪、为什么接受 ----

	// `pwd` 给 Windows 原生形式。这是**刻意**的:这一层的返回值要能直接喂给
	// ③ 层的 Windows 工具,而 POSIX 形式(`/d/...`)只有 Git Bash 系工具认。
	{script: `pwd`,
		known: "pwd 输出 Windows 原生形式(D:\\...),bash 给 MSYS 形式(/d/...)"},

	// EXIT trap:单条命令下与 bash 一致(mvdan/sh 在 Runner 结束时也会触发)。
	// 真正的差异在**跨命令**层面:winbash 是常驻会话,注册的 trap 会留给后面的命令,
	// 而 `bash -c` 每条都是新进程。那属于会话模型的必然结果,记在 progress 里。
	{script: `trap 'echo bye' EXIT; echo hi`},

	// 关联数组:迭代顺序**不保证**(bash 是哈希序、mvdan/sh 是插入序),所以语料只查
	// "键值都在、取得出",不比顺序 —— 依赖顺序的脚本本来就不该写。
	{script: `declare -A m; m[a]=1; m[b]=2; echo "${m[a]}-${m[b]}"`},
	// 但**元素个数**是 mvdan/sh 的缺口:`${#m[@]}` 对关联数组恒返回 1(bash 给真实个数),
	// 而键列表 `${!m[@]}` 是对的。这是静默给错值 —— 我们在这一层拦不住:
	// 要判断"这个变量是不是关联数组"得在运行期查变量表,AST 层面看不到。
	// 记在这里,顺带让"哪天 mvdan/sh 修好了"能被测试发现。
	{script: `declare -A m; m[a]=1; m[b]=2; echo "${#m[@]}"`,
		known: "mvdan/sh 的 ${#关联数组[@]} 恒为 1;键列表 ${!m[@]} 与取值都是对的"},

	// 未定义变量报错的**退出码**不同(bash 的 `-c` 走 127,我们给 1),
	// 错误文本也不带 `bash: line 1:` 前缀。两者都非零、都点名变量 —— 关键语义一致。
	{script: `set -u; echo ${never_set_xyz}`,
		known: "未定义变量:退出码 1 vs bash 的 127;我们的报错不带 bash: line N: 前缀"},
	{script: `echo ${never_set_xyz:?must set}`,
		known: "同上"},

	// `more` 在 Git Bash 里没有,而 Windows 的 System32 里有 —— 我们找到了它。
	// 这一条是"我们比 Git Bash 多一个工具",不是缺陷。
	{script: `echo hi | more`,
		known: "bash 报 more: command not found,我们用了 Windows 的 more.exe"},

	// 这两条 mvdan/sh **明确不支持**,我们的 ④ 层把它变成显式错误 ——
	// 正是设计意图(不认识的写法必须报错,不得静默降级)。已知差异,且是可取的差异。
	{script: `printf '%q\n' 'a b'`,
		known: "mvdan/sh 没有 %q 格式,我们报错而不是静默输出原样"},
	{script: `printf -v out '%s-%s' a b; echo "$out"`,
		known: "printf -v 不支持,我们报错并给出替代写法"},

	// ③ 层的重名工具(Windows System32 里也有 find.exe),`command -v` 与 `type` 是
	// mvdan/sh 的**内建**,它们按进程 PATH 回答,于是给 System32 那个。
	// 我们没有改写这两个内建:改写 `type` 要连别名/关键字的语义一起动,不划算。
	// 准确答案由 ① 层的 `which` 提供 —— 它给的就是我们真正会执行的那个。
	{script: `command -v find`,
		known: "mvdan/sh 的内建按进程 PATH 回答;准确答案用 winbash 的 `which find`"},
	{script: `type find`,
		known: "同上"},
	{script: `which find`,
		known: "我们的 which 给 Windows 路径形式(C:\\Program Files\\Git\\usr\\bin\\find.exe),指同一个文件"},

	// 锚定替换:mvdan/sh 没有实现且会**静默原样输出**,我们拦成显式错误。
	{script: `v=prefix-x; echo "${v/#prefix/root}"`,
		known: "锚定替换不支持;我们报错(mvdan/sh 会静默给错值)"},
	{script: `v=x-suffix; echo "${v/%suffix/tail}"`,
		known: "同上"},

	// `where` 是 Windows 工具(在 System32 里),它按**自己看到的 PATH** 报结果。
	// 我们给 ③ 层子进程的 PATH 与定位用的 searchPath 一致(usr\bin 在前),
	// 所以这条现在与 bash 一致 —— 留着当回归。
	{script: `where sort`},
}

// findRealBash 找真 Git Bash。找不到就跳过整组。
//
// 注意不能直接用 PATH 里的 `bash`:本机实测那个是**损坏的 WSL relay**
// (`execvpe(/bin/bash) failed`),拿它做参照系会把所有断言带偏。
//
// 也**不要**优先用 `C:\Program Files\Git\bin\bash.exe`:它只有 47 KB,是 Git for
// Windows 的**原生 launcher**(导入表里只有 KERNEL32/msvcrt/SHELL32/SHLWAPI/USER32,
// 没有 msys-2.0.dll),它把命令行**转发**给 usr\bin\bash.exe —— 那一跳用 MSVC 规则
// 重新拼,于是含双引号的脚本到真 bash 手里已经被吃掉了:
//
//	echo -e "a\tb"   → 真 bash 收到 `echo -e \a\tb\` → 输出 `atb\`
//
// 拿它当参照系,等于把"转发损坏"当成"真 bash 的行为",再把 winbash 的正确结果
// 判成错 —— 这正是本组测试一度 5 条红的原因。所以参照物只认 usr\bin 那份。
func findRealBash(t *testing.T) string {
	t.Helper()
	if usrBin := gitBashUsrBin(); usrBin != "" {
		if c := filepath.Join(usrBin, "bash.exe"); fileExists(c) {
			return c
		}
	}
	candidates := []string{
		`C:\Program Files\Git\usr\bin\bash.exe`,
		`C:\Program Files (x86)\Git\usr\bin\bash.exe`,
		`C:\Program Files\Git\bin\bash.exe`,
		`C:\Program Files (x86)\Git\bin\bash.exe`,
	}
	for _, c := range candidates {
		if fileExists(c) {
			return c
		}
	}
	t.Skip("这台机器上没有真 Git Bash,无法做真 bash 对照")
	return ""
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// runRealBash 跑一条语料,拿期望的 stdout 与退出码。
//
// 给参照子进程一份**显式环境**(见 cleanCorpusEnv):差分测试的价值完全取决于参照物
// 是否可信,而"真 bash"是会**被宿主环境改写**的 —— 有些 agent 沙箱会导出
// `safe-delete` 这类 shell 函数来包住 rm(靠 BASH_ENV 注入文件、靠导出的 BASH_FUNC_*
// 传函数)。一旦被改写,失败信息还会把人引向"winbash 的 rm 行为不对",误导性极强;
// 而且这类红是环境造成的,在干净机器上复现不了。
//
// winbash 侧不读这几个变量,这里只滤参照侧;加这个显式环境是为了让"两边环境一致"
// 这个意图写在代码里,免得以后有人往一边加环境依赖。
func runRealBash(t *testing.T, bash, script string) (string, int) {
	t.Helper()
	// 真 Git Bash 是 MSYS/Cygwin 程序:它用 build_argv 解析命令行,而 Go 默认的
	// syscall.EscapeArg 拼的是 MSVC CRT 规则 —— `\"` 到 build_argv 那里会变成
	// 「字面 \ + 引号结束」。于是**参照值先坏掉了**,失败信息还指着 winbash:
	//
	//	echo -e "a\tb"        bash 侧"期望"变成 atb\
	//	echo "$(pwd | wc -l)" bash 侧直接 exit=2(脚本文本已被损坏)
	//
	// 我们自己启动 MSYS 程序时早就是按它那套规则拼的(见 msysargv.go),参照侧同理。
	// 参照物不可信,整组差分测试就没有意义 —— 这正是 testenv 那个包存在的理由。
	cmd := exec.Command(bash, "-c", script)
	// 只有目标是**真的 MSYS 程序**时才用 build_argv 那套引号规则。usr\bin\bash.exe
	// 是(导入 msys-2.0.dll);一旦退回到 Git\bin\bash.exe 那个原生 launcher,就得用
	// Go 默认规则 —— 给它套 MSYS 引号,它会把单引号原样转发给下一跳,所有用例当场
	// exit=127(实测)。
	if isMSYSBinary(bash) {
		cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: strings.Join([]string{
			quoteMSYSArg(bash), "-c", quoteMSYSArg(script),
		}, " ")}
	}
	// 参照侧的 PATH 由我们给全:bash 所在的 usr\bin 必须在前,否则它连 ls 都找不到
	// (宿主从 PowerShell 跑 go test 时 PATH 里只有 Git\cmd)。理由见 testenv.WithPosixPath。
	cmd.Env = testenv.WithPosixPath(testenv.CleanBashEnv(), filepath.Dir(bash))
	cmd.Stdin = strings.NewReader("")
	out, err := cmd.Output()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running %q in %s: %v", script, bash, err)
	}
	return strings.TrimRight(string(out), " \t\r\n"), code
}

// cleanCorpusEnv 已挪到 internal/testenv(规则只有一条,实现只该有一份 —— 它原先在
// builtins 与 shell 两个包里被逐字抄了两遍)。
