// Package shell 把 mvdan/sh 的 interp 包装成一个可复用的会话。
//
// 一个 Session 持有 interp.Runner 的全部状态(变量、cwd、函数),因此连续多次 Run
// 之间的 shell 状态是保持的 —— 这正是 AI 在 Windows 上本来缺失的东西。
package shell

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"

	"winbash/internal/builtins"
	"winbash/internal/errs"
	"winbash/internal/pwsh"
	"winbash/internal/winmgmt"
)

// Session 是一次命令行"会话":它跨多次 Run 保持变量、cwd 与函数。
//
// 不保证并发安全(interp.Runner 本身也不安全)。
type Session struct {
	runner *interp.Runner

	outBuf, errBuf bytes.Buffer
	stdout, stderr *switchWriter

	// jsonOut 是全局的 --json 开关,② 层命令据此决定输出 TSV 还是类型化 JSON。
	// 放在 Session 上而不是每次 Run 传下去:命令实现只拿到 ctx,
	// 而开关属于"这一次调用",由调用方在 Run 时设定。
	jsonOut bool

	// engine 是 ② 层的常驻 pwsh,第一次真的用到才启动(见 psEngine)。
	// engineErr 缓存启动失败,避免每次调用都重付一次启动超时。
	engine    *pwsh.Conn
	engineErr error

	// searchPath 是 ③ 层要用的搜索路径:真实 PATH + Git Bash 的 usr\bin。
	// 它在构造时算一次,存下来 —— 进程环境不动(Global Constraints),
	// 所以这条路径只活在 Session 里。
	searchPath string
}

// switchWriter 是一个输出目标可替换的 io.Writer。
//
// interp.StdIO 只能在构造 Runner 时设置一次,但每次 Run 需要把输出收进不同的 buffer,
// 所以让 writer 本身持有一个可替换的目标。加锁是因为 interp 在后台命令(`cmd &`)下
// 可能并发写。
type switchWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *switchWriter) Set(w io.Writer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.w = w
}

func (s *switchWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.w == nil {
		return len(p), nil
	}
	return s.w.Write(p)
}

// EnvNested 是「正跑在 winbash 会话里」的标记,由 ② 层与 ③ 层派生给子进程。
//
// 为什么需要它:daemon 一条连接串行处理(见 daemon.Serve),会话里某条命令正在跑时,
// 若它又去连同一个 daemon,两边会互相等到 client 的 300s 超时 —— 实测
// `winbash -c 'winbash -c echo'` 正是这样挂住的。真 bash 的嵌套 bash 是两个独立进程,
// 所以这里也照办:带着标记的 client 改走本地一次性会话(client.Run 的嵌套分支)。
//
// 加一个环境变量不违反 Global Constraints 的「不修改用户的环境变量」:改的只是
// **我们派生的子进程**的 cmd.Env(与 envList 里把 PATH 换成 searchPath 是同一件事),
// 进程自己的 os.Environ() 一点没动;而且只有 winbash client 读这个名字。
const EnvNested = "WINBASH_SESSION"

// NewSession 建立一个会话,初始工作目录为当前进程的目录。
func NewSession() *Session {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}

	s := &Session{
		stdout:     &switchWriter{},
		stderr:     &switchWriter{},
		searchPath: BuildSearchPath(os.Getenv("PATH")),
	}

	// 滤掉继承来的嵌套标记。daemon 是 client 用 os/exec 拉起来的,而**嵌套里的**
	// client 拉起的 daemon 会带着这个标记启动 —— 若它进了 ① 层环境,标记就会经 daemon
	// 漏进之后每一条命令的子进程,于是「嵌套」成了常态:没嵌套的调用也走本地一次性会话,
	// 白白丢掉常驻带来的速度。
	env := os.Environ()
	kept := make([]string, 0, len(env))
	for _, kv := range env {
		if !envNameIs(kv, EnvNested) {
			kept = append(kept, kv)
		}
	}

	runner, err := interp.New(
		interp.Dir(cwd),
		interp.Env(expand.ListEnviron(kept...)),
		// stdin:daemon 目前没有把 client 的 stdin 转发过来,所以这里给一个**真正的空输入**,
		// 而不是 nil。nil 的 io.Reader 读起来是 panic —— 实测 `winbash -c 'grep xyz'`
		// (无文件、无管道)就是这样把 daemon 打崩的,client 只看到一句
		// "connection was forcibly closed",完全指不到根因。
		// 空输入则立即 EOF:读 stdin 的命令(`cat`、`grep`)立刻结束,不挂住、不报错。
		// 将来若要支持 `echo x | winbash -c 'cat'`,把这里换成从 client 转发的内容即可。
		interp.StdIO(bytes.NewReader(nil), s.stdout, s.stderr),
		interp.ExecHandler(s.execHandler),
		interp.CallHandler(s.callGuard),
	)
	if err != nil {
		// interp.New 只在 RunnerOption 非法时报错;上面的选项都是常量,出错即编程错误。
		panic("shell: interp.New: " + err.Error())
	}
	s.runner = runner
	return s
}

// execHandler 是四层路由的入口。
//
// 计划里的四层是:① 纯 Go 内建 → ② Go 内建但底层转常驻 pwsh → ③ 真实 PATH → ④ 报错 + 建议。
// 之所以在 ExecHandler 这一层做路由,而不是改写 AST:路由要能看见「这条命令
// 是否真的跑起来了」,这个信息只有执行期才有。
func (s *Session) execHandler(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return nil
	}
	hc := interp.HandlerCtx(ctx)

	// 把会话的搜索路径交给 ① 层(目前只有 `which` 需要它)。
	// 走 ctx 而不是改 Builtin 的形状 —— Registry 里只有一种形状是它的价值所在。
	ctx = builtins.WithSearchPath(ctx, s.searchPath)

	switch classify(args[0], s.searchPath) {
	case layerBuiltin:
		if code := builtins.Registry[args[0]](ctx, hc.Dir, args[1:], hc.Stdout, hc.Stderr); code != 0 {
			return interp.ExitStatus(code)
		}
		return nil

	case layerWinmgmt:
		conn, err := s.psEngine()
		if err != nil {
			fmt.Fprintln(hc.Stderr, errs.Errorf(args[0], err.Error(),
				"② 层靠 PowerShell 7 干活;先确认 pwsh 可用: winbash -c 'pwsh -NoProfile -c $PSVersionTable.PSVersion'"))
			return interp.ExitStatus(errs.ExitError)
		}
		// 把 ① 层当前的环境也带给 ② 层。常驻引擎继承的是**它启动那一刻**的环境,
		// 而 shell 里的 export 发生在之后 —— 不同步的话 `export X=1; pwsh -c '$env:X'`
		// 会静默给出空值(见 winmgmt/pwshcmd.go)。
		// 复用同一个 envList:③ 层子进程的环境与 ② 层的 $env: 因此保持一致。
		ctx = winmgmt.WithEnv(ctx, envList(hc.Env, s.searchPath))
		if code := winmgmt.Registry[args[0]](ctx, conn, args[1:], s.jsonOut, hc.Stdout, hc.Stderr); code != 0 {
			return interp.ExitStatus(code)
		}
		return nil
	}

	// ③/④ 层:自己定位、自己执行。
	// **不用 interp.DefaultExecHandler**(R3 决定):它内部走 interp.LookPath,
	// 读的是**进程 PATH**,不受我们构造的 searchPath 控制 —— 两者接不上,
	// 而 Global Constraints 不允许改环境变量。
	return s.runExternal(ctx, args, hc)
}

// callGuard 是所有简单命令在执行前都会经过的一道窄门。
//
// 它只做两件事,而且都限定在「mvdan/sh 的内建会**静默给出错结果**」这一种情况下:
//
//  1. `printf -v`:Windows 上不支持,原逻辑会把 `-v` 当普通参数原样输出。
//  2. `command -v X`:原逻辑只查**进程 PATH** 与它自己的内建表 —— 于是对
//     winbash 自己的内建(`ls`)和只在 Git Bash usr\bin 里的工具(`sed`)都会说"没有"。
//     AI 常写 `command -v X >/dev/null && …` 做存在性判断,这一误判是**静默**的。
//     这里把这种"必然答错"的调用改写成我们的 `which X`(它认识四层)——
//     其余一律不动,所以不存在语义回归。
func (s *Session) callGuard(ctx context.Context, args []string) ([]string, error) {
	if guarded, err := printfGuard(ctx, args); err != nil || len(guarded) == 0 {
		return guarded, err
	}

	if len(args) == 3 && args[0] == "command" && args[1] == "-v" {
		name := args[2]
		_, isBuiltin := builtins.Registry[name]
		_, isWinmgmt := winmgmt.Registry[name]
		if isBuiltin || isWinmgmt {
			return []string{"which", name}, nil
		}
		if _, err := exec.LookPath(name); err != nil {
			// 进程 PATH 里没有,但我们的搜索路径(git bash 的 usr\bin)里有:
			// 原逻辑会说"没有",而 winbash 明明跑得动它。
			if _, found := builtins.Which(name, s.searchPath); found {
				return []string{"which", name}, nil
			}
		}
	}

	// `echo -en …`:mvdan/sh 的 echo 只认单个 -n/-e/-E,拿到组合会把整串当**文本**打印。
	if expanded, ok := splitEchoFlags(args); ok {
		return expanded, nil
	}

	// `cd /c/Users` 与 `cd /`:cd 是 mvdan/sh 的内建,走不到我们的 OpenHandler,
	// 于是 POSIX 路径在这里一直失效(细则见 normalizeCdPath)。
	if len(args) == 2 && args[0] == "cd" {
		if fixed, ok := normalizeCdPath(interp.HandlerCtx(ctx).Dir, args[1]); ok {
			return []string{"cd", fixed}, nil
		}
	}

	return args, nil
}

// splitEchoFlags 把 `echo -en` 这类**组合开关**拆成 `-e -n`。
//
// mvdan/sh 的 echo 只认单个 -n/-e/-E;拿到组合它会把它当普通文本打印出来:
//
//	echo -en "a\nb"   →  "-en a\nb"     (真 bash 给两行 a / b)
//
// 这是静默错值,而 echo 是 AI 用得最多的命令。这里只动参数、不改 echo 的行为,
// 所以拆开之后走的还是 mvdan/sh 自己那套语义。
// 来源:oils spec `spec/builtin-echo.test.sh`。
func splitEchoFlags(args []string) ([]string, bool) {
	if len(args) < 2 || args[0] != "echo" {
		return nil, false
	}
	flags := args[1]
	if len(flags) < 3 || flags[0] != '-' {
		return nil, false // 单个 -n/-e/-E 本来就能用,不碰
	}
	for _, c := range flags[1:] {
		switch c {
		case 'e', 'n', 'E':
		default:
			return nil, false // 混了别的字符(例如 `-enx`)—— 交给 echo 自己报错
		}
	}

	out := make([]string, 0, len(args)+len(flags)-1)
	out = append(out, "echo")
	for _, c := range flags[1:] {
		out = append(out, "-"+string(c))
	}
	return append(out, args[2:]...), true
}

// normalizeCdPath 把 `cd` 的 POSIX 路径参数换成 Windows 形式。
//
// 为什么要在这里做:spec §8 要求"两种写法都接受",但**文件层**的两种写法是靠
// OpenHandler 拦的 —— `cd` 到不了那里,它是 mvdan/sh 的内建,直接改 runner 的当前目录。
// 实测后果(两条都是 oils spec 抓出来的):
//
//	cd /c/Users  →  cd: no such file or directory: "/c/Users"
//	cd /         →  **静默什么都不做**(`cd / && pwd` 打印的还是原目录)
//
// 只处理"以 / 开头"的写法:相对路径、`-`、`~`、以及 Windows 形式全部原样放行。
func normalizeCdPath(cwd, arg string) (string, bool) {
	if !strings.HasPrefix(arg, "/") {
		return "", false
	}
	if arg == "/" {
		// 单个 / 视作当前盘根:`cd /` 在 Windows 上最接近的语义就是它。
		if vol := filepath.VolumeName(cwd); vol != "" {
			return vol + `\`, true
		}
		return "", false
	}
	// `/c/Users/…` 这类 POSIX 盘符写法交给 ResolvePath —— 它已经被 ①/② 层共用,
	// 不必在这里再写一份路径规则。
	abs, err := builtins.ResolvePath(cwd, arg)
	if err != nil {
		return "", false
	}
	return abs, true
}

// printfGuard 拦住 mvdan/sh 在 Windows 上的静默缺口:`printf -v` 不被支持,// 而它**不报错** —— 会把 `-v` 当成普通参数原样输出,stdout 变成 "-v",
// 使用者看到命令"成功"了。这类"看起来成功了"的失败远比"命令找不到"恶劣(spec §13.3)。
//
// 用 CallHandler 而不是 ExecHandler:printf 是 mvdan/sh 的**内建**,
// 内建不经过 ExecHandler(那里只在「既不是函数也不是内建」时才被调用)——
// 想在交给内建之前插手,只有 CallHandler 这一条路。
func printfGuard(ctx context.Context, args []string) ([]string, error) {
	if len(args) >= 2 && args[0] == "printf" && args[1] == "-v" {
		return nil, errors.New(errs.Errorf("printf",
			"这台机器上不支持 printf -v(它会静默把 -v 当成普通参数输出)",
			`改用命令替换: var=$(printf '%s' 值)`))
	}
	return args, nil
}

// rejectProcessSubstitution 在**解析之后、运行之前**挡住 `<(cmd)` / `>(cmd)`。
//
// 为什么必须提前:Windows 上 mvdan/sh 未实现进程替换,而且会把
// `TODO: support process substitution on Windows` **打到 stdout** ——
// 那是一句看起来像输出、实际是未实现标记的东西,正是最恶劣的失败形态。
// 等运行到那一步才知道就晚了,所以这里走一遍 AST。
func rejectProcessSubstitution(prog *syntax.File) error {
	var found error
	syntax.Walk(prog, func(node syntax.Node) bool {
		if found != nil {
			return false
		}
		if ps, ok := node.(*syntax.ProcSubst); ok {
			form := "<(命令)"
			if ps.Op == syntax.CmdOut {
				form = ">(命令)"
			}
			found = errors.New(errs.Errorf("shell",
				"这台机器上不支持进程替换 "+form+"(mvdan/sh 未实现,且会把一句 TODO 打到 stdout)",
				"改用临时文件: 命令 > 临时文件,再对文件做下一步"))
			return false
		}
		return true
	})
	return found
}

// rejectUnsupportedParamExpansion 拦住 mvdan/sh 没有实现、而且**会静默给错值**的参数展开。
//
// 当前只有一条:bash 4 的锚定替换 —— `${v/#前缀/替换}`(只替换开头)与
// `${v/%后缀/替换}`(只替换结尾)。实测 mvdan/sh 把 `#前缀` 当成字面去找,
// 找不到就**原样输出未改动的值**、退出码为 0:语料扫描里 `v=prefix-x; echo ${v/#prefix/root}`
// 得到 `prefix-x`,而真 bash 给 `root-x`。
//
// 与 `<(cmd)` 同一类:不报错的失败比报错恶劣得多。所以拦在解析之后、运行之前。
// 注意别误伤 mvdan/sh **确实支持**的邻居:`${v#pre}`(去前缀)、`${v%post}`(去后缀)、
// `${v/x/y}`(普通替换)都不带这个锚定标记,不会命中这里。
func rejectUnsupportedParamExpansion(prog *syntax.File) error {
	var found error
	syntax.Walk(prog, func(node syntax.Node) bool {
		if found != nil {
			return false
		}
		pe, ok := node.(*syntax.ParamExp)
		if !ok || pe.Repl == nil {
			return true
		}
		switch wordHead(pe.Repl.Orig) {
		case "#":
			found = errors.New(errs.Errorf("shell",
				"这台机器上不支持 ${变量/#前缀/替换} 这类锚定替换(mvdan/sh 未实现,而且会静默原样输出)",
				`改用两步表达同一个意思: v=prefix-x; echo "root${v#prefix}"`))
		case "%":
			found = errors.New(errs.Errorf("shell",
				"这台机器上不支持 ${变量/%后缀/替换} 这类锚定替换(mvdan/sh 未实现,而且会静默原样输出)",
				`改用两步表达同一个意思: v=x-suffix; echo "${v%suffix}tail"`))
		}
		return true
	})
	return found
}

// wordHead 取一个词开头的字面首字符(只处理以字面开头的常见写法)。
func wordHead(w *syntax.Word) string {
	if w == nil || len(w.Parts) == 0 {
		return ""
	}
	lit, ok := w.Parts[0].(*syntax.Lit)
	if !ok || lit.Value == "" {
		return ""
	}
	return lit.Value[:1]
}

// tailWriter 把写出的内容原样转发,同时留下最后 limit 字节备用。
//
// 用途是**识别已知坑**:判断 `bash` 是不是那个损坏的 WSL relay,得看它说了什么。
// 但转发必须实时(不能等进程结束才把 stderr 吐出来),所以是"边转发边留尾巴"。
func tailWriter(w io.Writer, limit int) *tailBuffer {
	return &tailBuffer{w: w, limit: limit}
}

type tailBuffer struct {
	mu    sync.Mutex
	w     io.Writer
	buf   []byte
	limit int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.limit {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-t.limit:]...)
	}
	t.mu.Unlock()
	if t.w == nil {
		return len(p), nil
	}
	return t.w.Write(p)
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// runExternal 是 ③ 层(命中真实 PATH)与 ④ 层(哪个都没有)的实现。
func (s *Session) runExternal(ctx context.Context, args []string, hc interp.HandlerContext) error {
	abs, ok := builtins.Which(args[0], s.searchPath)
	if !ok {
		// ④ 层:明确报错 + 下一步。不是静默失败,也不是让 mvdan/sh 丢一句
		// "command not found" 了事 —— 那句话没告诉使用者接下来该做什么。
		fmt.Fprintln(hc.Stderr, errs.Errorf(args[0], "找不到这个命令",
			"确认命令名有没有写错;③ 层会搜索 Git Bash 的 usr\\bin 与真实 PATH,可用 winbash -c 'which "+args[0]+"' 自查"))
		return interp.ExitStatus(errs.ExitNotFound)
	}

	// MSYS 的**无扩展名脚本**(usr\bin 里有 44 个)不能直接交给 CreateProcess ——
	// Windows 只认 PE 文件。它们得由 MSYS 自己的 env.exe 启动:env 会读 shebang 找解释器,
	// 而这件事**不能**靠扩展名猜 —— `c_rehash` 是 Perl、`bzgrep` 是 sh。
	//
	// 试过但**不行**的两条路(都实测过):
	//
	//	`bash <script>`            → 强行用 shell 解析它,Perl 脚本会得到
	//	                              `line 15: my: command not found`
	//	`bash -c 'exec "$0" "$@"'` → 那串里的双引号在 Windows 命令行转义下会被搞坏,
	//	                              bash 报出 `cwd/$0 $@\` 这种怪名字
	//
	// 只在"目标没有扩展名"时才走这条,所以 .exe/.bat 的常规路径一点没被碰。
	argv0 := abs
	var leading []string
	if filepath.Ext(abs) == "" {
		envExe := ""
		if usrBin := gitBashUsrBin(); usrBin != "" {
			envExe = filepath.Join(usrBin, "env.exe")
		}
		if envExe == "" {
			fmt.Fprintln(hc.Stderr, errs.Errorf(args[0],
				"这是一个靠 shebang 运行的脚本(没有 .exe 扩展名),而 Windows 自己执行不了它",
				"这台机器上找不到 Git Bash;装上它(或让 "+`C:\Program Files\Git\usr\bin\env.exe`+" 存在)再试"))
			return interp.ExitStatus(errs.ExitNoExec)
		}
		// 反斜杠转成正斜杠:MSYS 认 `C:/…`。
		argv0, leading = envExe, []string{filepath.ToSlash(abs)}
	}

	// stderr 边转发边留尾:已知坑的识别靠它(见下),但输出不能延迟。
	tail := tailWriter(hc.Stderr, 4<<10)

	cmd := exec.CommandContext(ctx, argv0, append(append([]string{}, leading...), args[1:]...)...)
	cmd.Dir = hc.Dir
	cmd.Stdin = hc.Stdin
	cmd.Stdout = hc.Stdout
	cmd.Stderr = tail
	cmd.Env = envList(hc.Env, s.searchPath)

	err := cmd.Run()
	if err == nil {
		return nil
	}

	// 先翻译已知坑,再决定退出码:错误信息本身不该被改写,
	// 但使用者需要知道「这不是你写错了,是这台机器的环境有坑」。
	trapLine := ""
	if problem, remedy, ok := errs.TranslateKnownTrap(args[0], args[1:], tail.String()); ok {
		trapLine = "\n" + errs.Errorf(args[0], problem, remedy)
	}

	// 子进程自己的非零退出码原样带回去 —— 这是 bash 的语义(`grep 没匹配 → 1`),
	// 也是 ③ 层与 ④ 层的分界:跑起来了就不该替它改写结果。
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if trapLine != "" {
			fmt.Fprintln(hc.Stderr, trapLine)
		}
		return interp.ExitStatus(ee.ExitCode())
	}

	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(hc.Stderr, errs.Errorf(args[0], err.Error(), "文件已经不在了(它可能在两条命令之间被删掉或被换掉)")+trapLine)
		return interp.ExitStatus(errs.ExitNotFound)
	}

	fmt.Fprintln(hc.Stderr, errs.Errorf(args[0], err.Error(), "确认它是不是可执行文件,以及当前用户有没有权限运行它")+trapLine)
	return interp.ExitStatus(errs.ExitNoExec)
}

// envList 把 interp 的环境展开成 os/exec 要的形式,并把 PATH 换成我们构造的 searchPath。
//
// 用 hc.Env 而不是 os.Environ():shell 里的 `export FOO=bar` 必须对 ③ 层的子进程可见 ——
// 否则「在 winbash 里导出变量再跑 git」会是**静默无效**的,而那正是本项目要消灭的失败形态。
// 只带导出过的变量,与 POSIX 的语义一致(局部变量不给子进程)。
//
// PATH 必须换成 searchPath:③ 层用 searchPath **定位**命令,而子进程若拿到进程 PATH,
// 就会出现「定位用一条路径、执行用另一条」的分裂。实测后果:
//
//	`xargs -n1 echo`  →  /usr/bin/xargs: echo: No such file or directory
//	`ls | xargs -n1 echo | wc -l`  →  0(静默!应当是列数)
//
// 因为 xargs 要 exec 的 `echo` 只在 Git Bash 的 usr\bin 里。
// 这**不是**改进程环境:改的只是我们派生的子进程的 cmd.Env(Global Constraints 管的是前者)。
//
// 另外补两条:`MSYS=noglob`(理由见 msysWithNoGlob)与 EnvNested(理由见其文档)。
func envList(env expand.Environ, searchPath string) []string {
	var out []string
	sawPath := false
	msys := ""
	env.Each(func(name string, vr expand.Variable) bool {
		if name == "" || !vr.Exported || !vr.IsSet() {
			return true
		}
		switch {
		case strings.EqualFold(name, "PATH"):
			sawPath = true
			out = append(out, name+"="+searchPath)
		case strings.EqualFold(name, "MSYS"):
			// 先留着不写出去:下面要往这个值里加 noglob,调用方已有的选项得保留。
			msys = vr.String()
		case strings.EqualFold(name, EnvNested):
			// 跳过:下面统一补一条规范值。不跳的话会出现两条同名变量,
			// 而 Windows 上重复变量取哪一条没有保证。
		default:
			out = append(out, name+"="+vr.String())
		}
		return true
	})
	if !sawPath && searchPath != "" {
		// 环境里没有 PATH 时也得给一条,否则子进程连 system32 都找不到。
		out = append(out, "PATH="+searchPath)
	}
	return append(out, "MSYS="+msysWithNoGlob(msys), EnvNested+"=1")
}

// envNameIs 判断 "NAME=value" 里的名字是不是 want(Windows 上环境变量名不区分大小写)。
func envNameIs(kv, want string) bool {
	if i := strings.IndexByte(kv, '='); i >= 0 {
		return strings.EqualFold(kv[:i], want)
	}
	return false
}

// msysWithNoGlob 把 noglob 加进 MSYS 选项串(保留调用方已有的选项)。
//
// Cygwin/MSYS2 的 runtime 在「调用方不是 Cygwin 程序」时会**自己把 argv 里的通配符
// 展开一遍** —— 它怕原生 Windows 调用方漏了展开。我们从原生进程启动 /usr/bin/find,
// 于是引号本该保护的模式被它展开掉了:
//
//	find . -maxdepth 1 -name '*.go'
//	→ /usr/bin/find: paths must precede expression: `classify_test.go'
//	→ /usr/bin/find: possible unquoted pattern after predicate `-name'?
//
// 同一个命令在 Git Bash 里是好的:调用方也是 Cygwin 进程,runtime 不重复展开。
// `noglob` 关掉的正是这一层 —— 关掉之后 argv 由**我们**(真正的 shell)展开,
// MSYS 工具只负责收,行为与它们在真 bash 下一致。
func msysWithNoGlob(cur string) string {
	cur = strings.TrimSpace(cur)
	if cur == "" {
		return "noglob"
	}
	for _, opt := range strings.Split(cur, ":") {
		if strings.EqualFold(strings.TrimSpace(opt), "noglob") {
			return cur
		}
	}
	return cur + ":noglob"
}

// psEngine 返回 ② 层的常驻 pwsh,第一次真的用到时才启动。
//
// 惰性是有意的:只跑 ① 层的会话(例如 `ls | grep x`)不该付那次 ~350ms 的启动成本。
// 失败结果同样缓存 —— 否则 `svc list; svc status x` 会为同一个错误各等一次超时。
func (s *Session) psEngine() (*pwsh.Conn, error) {
	if s.engine != nil {
		// 引擎坏过一次就永远坏下去吗?不。帧失步(脚本写了主机流)或读写超时会让
		// 这个 Conn **单向**失效,但它换一个全新进程就能恢复 —— 实测一句
		// `Write-Host` 曾让该 daemon 的 ② 层此后全部报"会话已失效",只能重启 daemon。
		// 这里把坏的丢掉,走下面的重建路径。
		if s.engine.Dead() {
			_ = s.engine.Close()
			s.engine = nil
		} else {
			return s.engine, nil
		}
	}
	if s.engineErr != nil {
		return nil, s.engineErr
	}
	conn, err := pwsh.Start()
	if err != nil {
		s.engineErr = err
		return nil, err
	}
	s.engine = conn
	return conn, nil
}

// Close 释放会话持有的资源(目前只有 ② 层的 pwsh 引擎)。
//
// daemon 退出时必须调用:否则每重启一次 daemon 就留下一个常驻 pwsh 进程。
func (s *Session) Close() error {
	if s.engine == nil {
		return nil
	}
	err := s.engine.Close()
	s.engine = nil
	return err
}

// EngineStatus 报告 ② 层引擎的现状,给 --status 用。
//
// 「引擎起没起」是排错时第一个要问的问题:懒启动意味着一个只跑 ① 层的会话
// 根本没有 pwsh 子进程,而使用者看到的只是"svc 很慢"。
func (s *Session) EngineStatus() string {
	switch {
	case s.engine != nil:
		return fmt.Sprintf("pwsh=pid=%d calls=%d", s.engine.Pid(), s.engine.Calls())
	case s.engineErr != nil:
		return "pwsh=unavailable(" + s.engineErr.Error() + ")"
	default:
		return "pwsh=not-started"
	}
}

// Dir 返回会话当前的工作目录。cd 之后它会跟着变。
func (s *Session) Dir() string { return s.runner.Dir }

// Run 执行一段脚本,返回 stdout、退出码和错误行。
//
// 退出码沿用 shell 惯例:0 成功,非零失败;语法解析失败返回 2。
// jsonOut 是本次调用的 --json 开关,② 层命令据此改用类型化 JSON 输出。
func (s *Session) Run(ctx context.Context, script string, jsonOut bool) (stdout string, exit int, errs []string) {
	s.jsonOut = jsonOut

	s.outBuf.Reset()
	s.errBuf.Reset()
	s.stdout.Set(&s.outBuf)
	s.stderr.Set(&s.errBuf)
	defer func() {
		s.stdout.Set(nil)
		s.stderr.Set(nil)
	}()

	prog, perr := syntax.NewParser().Parse(strings.NewReader(script), "")
	if perr != nil {
		return "", 2, []string{perr.Error()}
	}
	// 解析之后、运行之前:拦住 Windows 上会静默降级的语法(进程替换、未实现的参数展开)。
	if rerr := rejectProcessSubstitution(prog); rerr != nil {
		return "", 2, []string{rerr.Error()}
	}
	if rerr := rejectUnsupportedParamExpansion(prog); rerr != nil {
		return "", 2, []string{rerr.Error()}
	}

	runErr := s.runner.Run(ctx, prog)

	if runErr != nil {
		var status interp.ExitStatus
		if errors.As(runErr, &status) {
			exit = int(status)
		} else {
			exit = 1
		}
	}

	errs = nonEmptyLines(s.errBuf.String())
	// 只有**不是** ExitStatus 的错误才追加:ExitStatus 是「命令跑完了、退出码非零」,
	// 不是 winbash 自己的错误。早先这里会把 mvdan/sh 的 "exit status 1" 当错误行附上去,
	// 于是 `grep 无匹配`、`exit 3` 这类再正常不过的结果都背了一行噪音(bash 不会这样打)。
	if runErr != nil && exit != 0 {
		var status interp.ExitStatus
		if !errors.As(runErr, &status) {
			errs = append(errs, runErr.Error())
		}
	}
	return s.outBuf.String(), exit, errs
}

// nonEmptyLines 把 stderr 拆成非空行,用于 Response.Errors。
func nonEmptyLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}
