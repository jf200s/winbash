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
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"

	"winbash/internal/builtins"
	"winbash/internal/errs"
	"winbash/internal/pwsh"
	"winbash/internal/winmgmt"
)

// dirOf 从 interp 的 handler context 里取当前目录(mvdan/sh 自己也是这么做的)。
func dirOf(ctx context.Context) string {
	return interp.HandlerCtx(ctx).Dir
}

// 三个 base handler 在包初始化时装一次 —— 它们是 mvdan/sh 的默认真实实现,
// 上面那三个包装器只做路径改写,其余全部委派回去。
var (
	baseOpenHandler    = interp.DefaultOpenHandler()
	baseStatHandler    = interp.DefaultStatHandler()
	baseReadDirHandler = interp.DefaultReadDirHandler2()
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

	// parser 是语法分析器,会话内复用。
	// mvdan 明确文档化「Parse can be called more than once, but not concurrently」
	// (third_party/mvdan-sh/syntax/parser.go),每次 Parse 开头都会 reset() 清掉
	// 上一次的解析状态,且每次调用传各自的 name —— 复用省掉的是每条命令一次
	// NewParser 的分配。为什么安全:会话内的请求严格串行(interp.Runner 本身也
	// 不保证并发安全,见 Session 的说明),不会撞上"不能并发"那一条。
	parser *syntax.Parser

	// searchPath 是 ③ 层要用的搜索路径:真实 PATH + Git Bash 的 usr\bin。
	// 它在构造时算一次,存下来 —— 进程环境不动(Global Constraints),
	// 所以这条路径只活在 Session 里。
	searchPath string

	// stdin 是本次 Run 的输入源(管道读端),Run 结束即关。见 SetStdinBytes。
	stdin *os.File

	// stdinW 是那根管道的**写端**,由 closeStdin 兜底关一次。
	//
	// 两条入口的分工不同,但写端都要有人管:
	//   - SetStdinBytes:Session 自己起 goroutine 消费它,goroutine 内部会关;
	//   - StdinPipe:把写端交给调用方(daemon)去喂,它有义务关。
	// 兜底这一下是幂等的(os.File.Close 二次调用只返回 ErrClosed),为的是
	// 调用方异常路径漏关时不会 fd/goroutine 泄漏。
	stdinW *os.File
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
// 为什么需要它:请求是投到**会话队列**里严格 FIFO 串行执行的(每个会话一个 worker,
// 见 daemon.serveSession),而不是整个 daemon 一把锁 —— 但会话里某条命令正在跑时,
// 若它又去连同一个 daemon 并落到**同一个会话**,两边仍会互相等到 client 的 300s 超时 —— 实测
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
		parser:     syntax.NewParser(),
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
		// stdin:没有调用 SetStdinBytes 时的默认输入 —— 一个**真正的空输入**,
		// 而不是 nil。nil 的 io.Reader 读起来是 panic —— 实测 `winbash -c 'grep xyz'`
		// (无文件、无管道)就是这样把 daemon 打崩的,client 只看到一句
		// "connection was forcibly closed",完全指不到根因。
		// 空输入则立即 EOF:读 stdin 的命令(`cat`、`grep`)立刻结束,不挂住、不报错。
		// 真要转发 client 的 stdin 请用 SetStdinBytes —— 这里传 bytes.Reader 是不行的,
		// mvdan/sh 会在**构造期**把非 *os.File 的 reader 抽干(详见 SetStdinBytes 的注释)。
		interp.StdIO(bytes.NewReader(nil), s.stdout, s.stderr),
		interp.ExecHandler(s.execHandler),
		interp.CallHandler(s.callGuard),
		// 下面三个 handler 都是**同一个原因**:mvdan/sh 判断绝对路径用 filepath.IsAbs,
		// 而它在 Windows 上不认 POSIX 形式,于是 `/tmp`、`/c/Users/x` 会被当成相对路径
		// 拼到 cwd 后面(实测 `echo hi > /tmp/t` → open <cwd>\tmp\t 找不到)。
		// 详见 builtins.ResolveShellPath 的注释。
		//
		// 默认 handler 装在包装器里而不是重写:相对路径的拼接、`/dev/null` 的 NUL 特例
		// 都保持 mvdan/sh 的原行为 —— 那两件事它做得对,不该被我们覆盖掉。
		interp.OpenHandler(func(ctx context.Context, path string, flag int, perm os.FileMode) (io.ReadWriteCloser, error) {
			resolved := builtins.ResolveShellPath(dirOf(ctx), path)
			f, err := baseOpenHandler(ctx, resolved, flag, perm)
			if err != nil {
				// 失败时换成 POSIX 措辞。要点:传的是**原始 path**(用户写的,
				// 如 `/nonexistent-dir/x`),不是上面转换后的 Windows 绝对路径 ——
				// 那个只作为 absPath 用于区分 `Not a directory`,绝不进输出。
				//
				// 这条路径覆盖:输出/追加重定向、输入重定向、以及 source 找不到文件。
				// 它原先原样端出 Go 的错误(`open C:\...: The system cannot find the
				// path specified.`)—— 泄漏 Win32 API 名与绝对路径,且不是 POSIX 措辞。
				return nil, builtins.ShellPathError(path, err, resolved)
			}
			// 打开**成功**但目标是目录:Windows 允许 open 目录,失败要等到第一次 Read。
			// 在那里报错会走出来不及措辞的路径(见 builtins/dirread.go 的长注释),
			// 所以在这里就拦掉 —— 得到的措辞与"重定向失败"天然一致。
			return builtins.OpenErrorOrDir(f, path, resolved)
		}),
		interp.StatHandler(func(ctx context.Context, path string, follow bool) (fs.FileInfo, error) {
			dir := dirOf(ctx)
			fixed := builtins.ResolveShellPath(dir, path)
			if fixed == path {
				// `/dev/null` 特判(mvdan/sh 的 r.stat 先把路径拼成 `<dir>\dev\null`
				// 才交给 handler,原始写法在那一步就丢了)。**事实优先**:真的存在
				// 这个路径就采信它 —— 用户在 cwd 建了 `dev/null` 就该按事实处理,
				// 与 RestoreSyntheticMount 的既有约定一致。不存在才当 NUL,
				// 返回手工构造的字符设备 FileInfo(不是 os.Stat("NUL") 的结果,
				// 否则 -f/-c 恰好全反,见 devnull.go)。
				if _, err := os.Lstat(path); err != nil {
					if info, ok := devNullSynthetic(dir, path); ok {
						return info, nil
					}
				}
				// mvdan/sh 的 `r.stat` 会先把路径拼成 `<dir>\tmp` 才交给 handler,
				// 原始写法在那一步就丢了 —— 拿不回来只能还原(见 RestoreSyntheticMount)。
				fixed = builtins.RestoreSyntheticMount(dir, path)
			}
			return baseStatHandler(ctx, fixed, follow)
		}),
		interp.ReadDirHandler2(func(ctx context.Context, path string) ([]fs.DirEntry, error) {
			return baseReadDirHandler(ctx, builtins.ResolveShellPath(dirOf(ctx), path))
		}),
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

	// locate 一次判定 + (裸名字)定位,(层, 绝对路径)一起带走 —— 判层与下面的
	// runExternal 共享同一次 PATH 扫描,不再各扫一遍(见 locate 顶上的注释)。
	lay, abs := locate(args[0], s.searchPath)
	switch lay {
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
		// 这两个 ctx 值只有 `pwsh` 子命令会取用:svc/reg/ps/sys 是固定脚本,
		// 走 conn.Run,不读 cwd/env(envFrom/dirFrom 的调用点只在 pwshcmd.go:137,169)。
		// 为什么安全:对那四条命令,附上去的值从来没人读 —— 白做;按名字收进 pwsh
		// 分支后行为零变化。代价是 shell 包硬编码了 "pwsh" 这个名字:将来 ② 层若有
		// 新命令要读 env/dir,必须同步这里(两条注释互指)。
		if args[0] == "pwsh" {
			ctx = winmgmt.WithEnv(ctx, envList(hc.Env, s.searchPath))
			// 引擎常驻,所以 cwd 也必须每条同步(与 env 同一个理由):它的 cwd 是 daemon
			// 启动时继承的那个,不会跟着会话的 `cd` 走。
			ctx = winmgmt.WithDir(ctx, hc.Dir)
		}
		if code := winmgmt.Registry[args[0]](ctx, conn, args[1:], s.jsonOut, hc.Stdout, hc.Stderr); code != 0 {
			return interp.ExitStatus(code)
		}
		return nil
	}

	// ③/④ 层:自己定位、自己执行。
	// **不用 interp.DefaultExecHandler**(R3 决定):它内部走 interp.LookPath,
	// 读的是**进程 PATH**,不受我们构造的 searchPath 控制 —— 两者接不上,
	// 而 Global Constraints 不允许改环境变量。
	// locate 的结果(绝对路径 / 是否命中)一起带下去,runExternal 对裸名字
	// 直接采用,不再对同一个 searchPath 扫第二遍。
	return s.runExternal(ctx, args, hc, abs, lay == layerPath)
}

// callGuard 是所有简单命令在执行前都会经过的一道窄门。
//
// 它只处理「mvdan/sh 的内建会**静默给出错结果**」这一类情况,而且是逐条列举的,
// 不做通用改写:
//
//  1. `printf -v`:Windows 上不支持,原逻辑会把 `-v` 当普通参数原样输出。
//  2. `trap ... DEBUG` / `trap ... RETURN`:mvdan/sh 不认这两个信号,而它自己只把
//     `invalid signal specification` 写到 stderr、**退出码仍是 0**、hook 静默不执行。
//     详见 trapGuard 的注释。
//  3. `command -v X`:原逻辑只查**进程 PATH** 与它自己的内建表 —— 于是对
//     winbash 自己的内建(`ls`)和只在 Git Bash usr\bin 里的工具(`sed`)都会说"没有"。
//     AI 常写 `command -v X >/dev/null && …` 做存在性判断,这一误判是**静默**的。
//     这里把这种"必然答错"的调用改写成我们的 `which X`(它认识四层)——
//     其余一律不动,所以不存在语义回归。
func (s *Session) callGuard(ctx context.Context, args []string) ([]string, error) {
	if guarded, err := printfGuard(ctx, args); err != nil || len(guarded) == 0 {
		return guarded, err
	}
	if guarded, err := trapGuard(args); err != nil || len(guarded) == 0 {
		return guarded, err
	}

	// kill:mvdan/sh 把它列进内建名表却没有实现 —— dispatch 落到 default 分支恒
	// failf(2, "unsupported builtin"),而内建不走 ExecHandler,③ 层 Git Bash 自带的
	// kill.exe 根本轮不到。killGuard 把它翻译成 Windows 语义(taskkill)。细则见
	// killguard.go 顶上的注释。
	if rew, ok := killGuard(ctx, args); ok {
		return rew, nil
	}

	// `command -v X [X...]`:mvdan/sh 的原逻辑只查**进程 PATH** 与它自己的内建表 ——
	// 于是对 winbash 自己的内建(`ls`)和只在 Git Bash usr\bin 里的工具(`sed`)都会说
	// "没有"。AI 常写 `command -v X >/dev/null && …` 做存在性判断,这一误判是**静默**的。
	//
	// 判据必须覆盖**多名字**写法(`command -v a b`)。早先只写 `len(args)==3`,
	// 于是多名字那条整条漏拦:实测 `command -v sed cat`(PATH 里没有 usr\bin 时)
	// 无任何输出、exit 1,而 sed 与 cat 都跑得动 —— 调用方的 `&&` 会静默短路。
	//
	// 为什么是「全部都要我们插手才换」:CallHandler 只能返回**一条**命令的 argv,
	// 没法把 `command -v a b` 拆成两条命令依次执行。所以只在这条命令**完全**用得上
	// 我们的时候整体换成 `which a b`(它本来就吃多名字,输出形态也与 `command -v` 一致:
	// ①/② 层报名字、③ 层报绝对路径)。
	//
	// 混合写法(既有内建又有 mvdan 自己答得对的,如 `command -v ls git`)只能**放行**
	// —— 那样 ls 仍会被漏报。这是已知残余缺口,已记进 README:真要逐个判存在,
	// 就分开写 `command -v ls && command -v git`。
	if len(args) >= 3 && args[0] == "command" && args[1] == "-v" {
		names := args[2:]
		allOurs := true
		for _, name := range names {
			if !s.needsOurWhich(name) {
				allOurs = false
				break
			}
		}
		if allOurs {
			return append([]string{"which"}, names...), nil
		}
	}

	// `echo -en …`:mvdan/sh 的 echo 只认单个 -n/-e/-E,拿到组合会把整串当**文本**打印。
	if expanded, ok := splitEchoFlags(args); ok {
		return expanded, nil
	}

	// `cd /c/Users` 与 `cd /`:cd 是 mvdan/sh 的内建,走不到我们的 OpenHandler,
	// 于是 POSIX 路径在这里一直失效(细则见 normalizeCdPath)。
	//
	// 判据必须覆盖**别名前缀**:实测 `command cd /` 与 `builtin cd /c/Users` 都绕过了
	// 这里(原先只认 args[0]=="cd"),后果比"不起效"更糟 —— 实测二者都返回 **0**,
	// 而 cwd 要么没动、要么被拼成 `<cwd>\c\Users`(一个不存在的相对路径)。
	// 也就是说脚本以为切过去了、后面全在错的目录里跑,全程没有任何报错。
	if fixed, prefix, ok := normalizeCdInvocation(interp.HandlerCtx(ctx).Dir, args); ok {
		return append(append([]string{}, prefix...), "cd", fixed), nil
	}

	return args, nil
}

// normalizeCdInvocation 认出 `cd X` / `command cd X` / `builtin cd X` 三种形态,
// 返回规范化后的路径与需要保留的前缀。
//
// 为什么把这三种收在一处:它们走的是**同一个** mvdan/sh 内建,也就会踩同一个
// 「POSIX 路径没被规范化」的坑。原先只处理 `cd X`,于是 `command cd /` 与
// `builtin cd /c/Users` 静默给出错结果 —— 实测两者都返回 0:
//
//	command cd /        → cwd 没动(仍是原目录)
//	builtin cd /c/Users → cwd 变成 <原 cwd>\c\Users(一个不存在的相对路径)
//
// 返回值 prefix 是要保留的前缀(command / builtin),保留它是为了不改语义:
// 那两种写法本来就是在**刻意绕过**函数或别名,替换成裸 `cd` 会把这一层去掉。
func normalizeCdInvocation(dir string, args []string) (fixed string, prefix []string, ok bool) {
	var target string
	switch {
	case len(args) == 2 && args[0] == "cd":
		target = args[1]
	case len(args) == 3 && (args[0] == "command" || args[0] == "builtin") && args[1] == "cd":
		prefix = []string{args[0]}
		target = args[2]
	default:
		return "", nil, false
	}
	fixed, ok = normalizeCdPath(dir, target)
	if !ok {
		return "", nil, false
	}
	return fixed, prefix, true
}

// needsOurWhich 报告这个名字是不是「只有 winbash 认识」—— 即 mvdan/sh 的
// `command -v` 必然答错的那几种。
//
// 三种:
//  1. ① 层内建(`ls`/`cat`/`grep`/`which`):不在进程 PATH 里,mvdan/sh 会说"没有";
//  2. ② 层内建(`svc`/`reg`/`ps`/`sys`/`pwsh`):同上;
//  3. 只存在于 Git Bash `usr\bin` 里的工具(`sed`/`awk`/`find`…):宿主 PATH 不一定有它,
//     而 winbash 的第 ③ 层能跑 —— 这也是「AI 写 `command -v sed && …` 却静默走 else」的根源。
//
// 其余一律返回 false,交给 mvdan/sh 自己答。这一条很关键:函数、别名、PATH 里的命令
// 它答得对,我们把它们也换掉只会引入新语义(比如把函数名换成 which 就答"找不到")。
func (s *Session) needsOurWhich(name string) bool {
	if name == "" {
		return false
	}
	if _, ok := builtins.Registry[name]; ok {
		return true
	}
	if _, ok := winmgmt.Registry[name]; ok {
		return true
	}
	// 进程 PATH 里有 → mvdan/sh 自己答得对,不插手。
	if _, err := exec.LookPath(name); err == nil {
		return false
	}
	// 进程 PATH 里没有,但我们的搜索路径(Git Bash 的 usr\bin + 真 PATH)里有。
	// 走 locate 单入口,与判层/执行共享同一次扫描(以及负缓存);对这里的判定
	// 语义无影响 —— locate 调的就是同一个 Which,miss 也照样答"不归我们管"。
	if lay, _ := locate(name, s.searchPath); lay == layerPath {
		return true
	}
	return false
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
	// `/c/...`(盘符)与 `/tmp`、`/usr/bin`、`/bin`、`/`(MSYS 挂载点)都交给
	// ResolvePath —— 它已经被 ①/② 层共用,路径规则只该有一份。
	//
	// 注意 `cd /` 现在解析到 **Git 安装根**(真 bash 里 `/` 就是那里),
	// 而不是"当前盘根"。早先这里单独把 `/` 特判成盘根(`D:\`),那与真 bash 不一致:
	// 实测真 bash 的 `cd / && ls` 能看到 `usr/ etc/ bin/`,而盘根看不到。
	// 挂载表在 msyspath.go,这一条是它的自然结果,所以特判删掉。
	abs, err := builtins.ResolvePath(cwd, arg)
	if err != nil {
		return "", false
	}
	if abs == arg {
		return "", false // 没改写就别动它(交回 mvdan/sh 原语义)
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

// trapGuard 拦住 mvdan/sh 认不了的所有 trap 信号。
//
// 它只支持 **ERR** 与 **EXIT** 两个(源码里就是一个 switch,default 分支 failf)。
// 其余信号(INT / TERM / USR1 / HUP / DEBUG / RETURN …)的行为是:
//
//	trap 'echo d' DEBUG; echo x
//	→ stderr: trap: DEBUG: invalid signal specification
//	→ stdout: x        (hook 静默不执行)
//	→ 整体 exit 0
//
// 也就是说 **stderr 有话说、退出码却是 0**。调用方只看退出码会以为 hook 装上了 ——
// 而「把信号设成空 handler」那种"想忽略 Ctrl+C"的写法更是彻底失效:一个字节都没生效。
// 这是本项目要消灭的静默形态,所以这里统一拦成响亮失败。
//
// 拦法沿用 printf -v 的先例:三行式 + 替代写法,不去猜一个近似语义。
// 为什么不实现:信号处理要在每条命令/函数返回点重入 interp.Runner,会搅乱 runner
// 状态机(嵌套 Run、变量作用域、trap 自身的重入保护),风险与收益不成比例。
//
// 判定边界:只有出现在**信号位置**的名字才算。`trap 'echo DEBUG' INT` 里的 DEBUG
// 是 callback 文本,不该被误拦 —— 所以先按 bash 的 `trap [[arg] sigspec ...]` 切出
// 信号列表,再逐个比对白名单。
func trapGuard(args []string) ([]string, error) {
	if len(args) < 2 || args[0] != "trap" {
		// `trap` 无参数在 bash 里是"列出当前 trap";mvdan/sh 静默返回空。
		// 这里不拦:它不产生错误结果,只是少了个查询功能(记在 README 的已知差异里)。
		return args, nil
	}
	// `-l` / `-p` 是真 flag,上游自己报 NOT IMPLEMENTED(exit 2),响亮,不插手。
	// 注意 `-` 单独出现**不是** flag,而是 arg 位置上的"重置"(见下)。
	if args[1] == "-l" || args[1] == "-p" {
		return args, nil
	}

	// 按 bash 的 `trap [[arg] sigspec ...]` 切出信号列表:
	//   trap INT              两个参数 → 第二个就是信号
	//   trap '重置' -          三个参数 → args[1] 是 arg,信号从 args[2] 起
	//   trap - INT            同上,`-` 是"恢复默认"的 arg,INT 仍是信号(要拦)
	sigs := args[1:]
	if len(args) >= 3 {
		sigs = args[2:]
	}
	for _, sig := range sigs {
		if sig == "ERR" || sig == "EXIT" {
			continue // mvdan/sh 真正支持的两个
		}
		return nil, errors.New(errs.Errorf("trap",
			"这台机器上不支持 trap "+sig+
				"(mvdan/sh 只实现 ERR 与 EXIT;传其它信号会报 invalid signal specification,"+
				"但退出码仍是 0、hook 静默不执行)",
			trapRemedy(sig)))
	}
	return args, nil
}

// trapRemedy 给每种常见信号一句能落地的替代写法 —— 只报"不支持"等于把人留在原地。
func trapRemedy(sig string) string {
	switch sig {
	case "DEBUG":
		return `若只是想看每条命令,用 set -x(set +x 关掉);要"每条命令前做某事",就把那件事显式写在命令之间`
	case "RETURN":
		return `把收尾动作直接写在函数的最后一行,别靠 RETURN hook`
	case "INT", "TERM", "HUP", "QUIT", "WINCH":
		return `信号处理在本实现里不存在;要"外部打断时清理",用 trap ... EXIT(它支持),或让命令自己处理超时后退出`
	default:
		return `改用 trap ... EXIT(唯一能覆盖"结束时做某事"的信号),或把动作显式写在命令之间`
	}
}

// rejectUnsupportedSyntax 在**解析之后、运行之前**挡住 Windows 上会静默降级的语法。
// 三个关注点看的是同一棵树,原先各自 Walk 一遍纯属重复,这里合并成一次遍历;
// 前两个检查的职责与报错文案原样保留:
//
//  1. 进程替换 `<(cmd)` / `>(cmd)` —— 为什么必须提前:Windows 上 mvdan/sh 未实现
//     进程替换,而且会把 `TODO: support process substitution on Windows`
//     **打到 stdout** —— 那是一句看起来像输出、实际是未实现标记的东西,正是最恶劣的
//     失败形态。等运行到那一步才知道就晚了,所以这里走一遍 AST。
//  2. 锚定参数展开 `${v/#前缀/替换}` 与 `${v/%后缀/替换}` —— 实测 mvdan/sh 把
//     `#前缀` 当成字面去找,找不到就**原样输出未改动的值**、退出码为 0:
//     语料扫描里 `v=prefix-x; echo ${v/#prefix/root}` 得到 `prefix-x`,而真 bash
//     给 `root-x`。与 `<(cmd)` 同一类:不报错的失败比报错恶劣得多。
//  3. **收集性信息**(不报错、不剪枝):脚本里是否有后台任务(`cmd &`)。它是
//     2026-10-31 会话语义决议的输入:后台 job 随请求结束被取消(差异表已记账),
//     RunArgs 据此在响应 errors 里追加一行**指路 `--detach`** 的警告 —— 只报存在
//     与否,整个脚本至多一行,不是错误、不改退出码。
//
// 为什么安全(与原先两遍遍历逐点等价):进程替换报 DFS 首个,锚定展开也报 DFS 首个、
// 且原先第二遍会下钻进 ProcSubst 的子树,这里照旧不剪枝;裁决顺序沿用原先 RunArgs
// 的检查顺序 —— 有进程替换先报进程替换,否则报锚定展开。
// 返回的 hasBackground 只在 err == nil 时可信:两个错误都命中后遍历会提前收摊,
// 但那时脚本已被整体拒绝,警告无从谈起。
func rejectUnsupportedSyntax(prog *syntax.File) (hasBackground bool, err error) {
	var psErr, peErr error
	syntax.Walk(prog, func(node syntax.Node) bool {
		if psErr != nil && peErr != nil {
			return false
		}
		switch n := node.(type) {
		case *syntax.Stmt:
			// `cmd &` 在 mvdan 的 AST 里不是 BinaryCmd,而是 Stmt 的 Background 字段
			// (syntax/nodes.go:「Background bool // stmt &」—— 实测探针确认:
			// `nohup cmd &`、`( … ) &`、`a | b &`、`a & b`、`if …; then x & fi`
			// 全部落在这一处;`a && b` 是 AndExpr,不置 Background,不会误伤)。
			if n.Background {
				hasBackground = true
			}
		case *syntax.ProcSubst:
			if psErr == nil {
				form := "<(命令)"
				if n.Op == syntax.CmdOut {
					form = ">(命令)"
				}
				psErr = errors.New(errs.Errorf("shell",
					"这台机器上不支持进程替换 "+form+"(mvdan/sh 未实现,且会把一句 TODO 打到 stdout)",
					"改用临时文件: 命令 > 临时文件,再对文件做下一步"))
			}
			// 刻意不在这里剪枝:锚定展开可能嵌在进程替换里(如 `cat <(${v/#a/b})`),
			// 原先第二遍看得到它,这里也要看得到,「同时有两种坏语法」时报错对象才一致。
			return true
		case *syntax.ParamExp:
			// 注意别误伤 mvdan/sh **确实支持**的邻居:`${v#pre}`(去前缀)、
			// `${v%post}`(去后缀)、`${v/x/y}`(普通替换)都不带这个锚定标记,
			// 不会命中这里。
			if peErr == nil && n.Repl != nil {
				switch wordHead(n.Repl.Orig) {
				case "#":
					peErr = errors.New(errs.Errorf("shell",
						"这台机器上不支持 ${变量/#前缀/替换} 这类锚定替换(mvdan/sh 未实现,而且会静默原样输出)",
						`改用两步表达同一个意思: v=prefix-x; echo "root${v#prefix}"`))
				case "%":
					peErr = errors.New(errs.Errorf("shell",
						"这台机器上不支持 ${变量/%后缀/替换} 这类锚定替换(mvdan/sh 未实现,而且会静默原样输出)",
						`改用两步表达同一个意思: v=x-suffix; echo "${v%suffix}tail"`))
				}
			}
		}
		return true
	})
	if psErr != nil {
		return hasBackground, psErr
	}
	return hasBackground, peErr
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
//
// preAbs / preOK 是 execHandler 从 locate 带下来的定位结果:裸名字(不含 / 或 \)
// 直接采用,不再对同一个 searchPath 扫第二遍 —— 这是判层与执行共享同一次扫描的
// 接缝。路径写法忽略这两个值:locate 那边的 stat 基准是**进程 cwd**,这里要按
// 会话 cwd 重新解析后再定位(就是下面这段)。
func (s *Session) runExternal(ctx context.Context, args []string, hc interp.HandlerContext, preAbs string, preOK bool) error {
	// args[0] 有三种形态:裸名字(sed)、自带路径的相对写法(./t.sh)、自带路径的
	// 绝对写法(/c/x/y.sh、C:\x\y.exe)。Which 对"自带路径"直接 os.Stat,而那个 stat
	// 的基准是**进程** cwd(daemon 启动时那个目录),**不是**会话 cwd(hc.Dir ——
	// 会话里的 `cd` 只改 interp 的状态,要到最后才通过 cmd.Dir 交给子进程)。
	//
	// 两者通常不同,于是 `cd /tmp && ./t.sh` 会去 daemon 的目录里找 ./t.sh,报
	// 「找不到这个命令」,而文案还把人引向"命令名写错了"(实测 exit 127)。
	// 所以先按会话 cwd 解析成 Windows 绝对路径,再交给 Which。
	target := args[0]
	var abs string
	var ok bool
	if strings.ContainsAny(target, `/\`) {
		// ResolveShellPath 负责 MSYS 挂载表(/c、/tmp、/usr/bin …);剩下的相对路径
		// 按会话 cwd 拼成绝对路径。
		target = builtins.ResolveShellPath(hc.Dir, target)
		if !filepath.IsAbs(target) {
			target = filepath.Join(hc.Dir, target)
		}
		abs, ok = builtins.Which(target, s.searchPath)
	} else {
		// 裸名字:locate 那一次扫描的结果直接带走(含负缓存判出的 miss)。
		abs, ok = preAbs, preOK
	}
	if !ok {
		// ④ 层:明确报错 + 下一步。不是静默失败,也不是让 mvdan/sh 丢一句
		// "command not found" 了事 —— 那句话没告诉使用者接下来该做什么。
		hint := "确认命令名有没有写错;③ 层会搜索 Git Bash 的 usr\\bin 与真实 PATH,可用 winbash -c 'which " + args[0] + "' 自查"
		if target != args[0] {
			// 自带路径的写法:把它解析成了什么也说出来,否则使用者不知道自己写的
			// 相对路径被按哪个目录解释了。
			hint = "按会话目录解析成 " + target + " 之后仍然找不到;确认它存在: ls '" + target + "'"
		}
		fmt.Fprintln(hc.Stderr, errs.Errorf(args[0], "找不到这个命令", hint))
		return interp.ExitStatus(errs.ExitNotFound)
	}

	// Windows 只能 CreateProcess 一个 **PE 映像**,判据是文件头而不是扩展名 ——
	// 见 isPEImage 顶上那两条实测(`./t.sh` 报 126、`c_rehash` 是 Perl 无扩展名)。
	// 不是 PE 的就交给 MSYS 的 env.exe:env 读 shebang 找解释器,而这件事**不能**
	// 靠扩展名猜(usr\bin 里 bzgrep 是 sh、c_rehash 是 Perl)。
	//
	// 试过但**不行**的两条路(都实测过):
	//
	//	`bash <script>`            → 强行用 shell 解析它,Perl 脚本会得到
	//	                              `line 15: my: command not found`
	//	`bash -c 'exec "$0" "$@"'` → 那串里的双引号在 Windows 命令行转义下会被搞坏,
	//	                              bash 报出 `cwd/$0 $@\` 这种怪名字
	argv0 := abs
	var leading []string
	ext := strings.ToLower(filepath.Ext(abs))
	if !isPEImage(abs) {
		if ext == ".bat" || ext == ".cmd" {
			// 批处理既不是 PE、也没有 shebang:CreateProcess 与 env.exe 都跑不了它。
			// 但 npm / pnpm / tsc 这些工具的入口正是 .cmd 垫片,一律拒绝等于把整个
			// Node 生态挡在门外。所以先试**翻译**:认出「垫片在调哪个 node 脚本」
			// 就直接 exec 那个脚本,使用者传来的参数一个字节都不经过 cmd。
			// 认不出来才保持原来的拒绝(不猜)。
			if call, ok := resolveNodeShim(abs, s.searchPath); ok {
				argv0 = call.Prog
				leading = call.Args
			} else if cmdShellFallbackEnabled(os.Getenv) {
				// 兜底:显式打开的开关。代价是参数交给 cmd 重新解析
				// (`&` `|` `^` `%VAR%` 都会被解释),所以它默认关。
				argv0 = os.Getenv("COMSPEC")
				if argv0 == "" {
					argv0 = `C:\Windows\System32\cmd.exe`
				}
				leading = []string{"/c", abs}
			} else {
				fmt.Fprintln(hc.Stderr, errs.Errorf(args[0],
					"Windows 批处理("+ext+")不能直接执行,且这条垫片不是可识别的 npm/node 形状",
					"用 `cmd /c ...` 显式调它;若确认要 winbash 代劳,先 `export "+envCmdShellFallback+
						"=1`(代价:参数会经 cmd 的解析器,`&` `|` `^` `%VAR%` 会被重新解释)"))
				return interp.ExitStatus(errs.ExitNoExec)
			}
		} else {
			envExe := ""
			if usrBin := gitBashUsrBin(); usrBin != "" {
				envExe = filepath.Join(usrBin, "env.exe")
			}
			if envExe == "" {
				fmt.Fprintln(hc.Stderr, errs.Errorf(args[0],
					"这是一个靠 shebang 运行的脚本,而 Windows 自己执行不了它",
					"这台机器上找不到 Git Bash;装上它(或让 "+`C:\Program Files\Git\usr\bin\env.exe`+" 存在)再试"))
				return interp.ExitStatus(errs.ExitNoExec)
			}
			// 反斜杠转成正斜杠:MSYS 认 `C:/…`。
			//
			// 脚本参数给**用户写的原串**,不是解析出来的绝对路径:它一路传到 MSYS 的
			// bash 子进程当 $0,而 bash 运行时报错的前缀用的正是 $0 —— 传绝对路径,
			// `./script.sh: line 1: foo: command not found` 就变成
			// `C:/Users/…/script.sh: line 1: …`,真 bash 打的是用户写的那个。
			// 两条例外,都传 abs:其一,挂载解析(`/tmp/x.sh`、`/c/x/y.sh`)改写过的 ——
			// 合成挂载与 bash 自己的 MSYS 挂载表可能分叉,原串未必指向同一个文件;
			// 其二,裸名字(`sed`、PATH 里的脚本)—— 原串要靠子进程自己搜 PATH,而它的
			// PATH 是我们合成的 searchPath,搜索语义可能与定位时不同,原串未必可定位。
			// 相对路径(`./x.sh`、`sub/x.sh`)则由子进程 cwd(= hc.Dir,见下 cmd.Dir)
			// 兜底,一定能找到文件。
			scriptArg := filepath.ToSlash(abs)
			if strings.ContainsAny(args[0], `/\`) && builtins.ResolveShellPath(hc.Dir, args[0]) == args[0] {
				scriptArg = args[0]
			}
			argv0, leading = envExe, []string{scriptArg}
		}
	}

	// stderr 边转发边留尾:已知坑的识别靠它(见下),但输出不能延迟。
	tail := tailWriter(hc.Stderr, 4<<10)

	argv := append(append([]string{}, leading...), args[1:]...)
	cmd := exec.CommandContext(ctx, argv0, argv...)
	// MSYS/Cygwin 目标要用**它自己的**引号规则拼命令行,不能交给 Go 默认的
	// MSVC 规则(理由与实测见 msysargv.go 开头)。原生目标一行不改 —— 反过来给
	// node / git 套上 MSYS 引号,它们会收到带引号的字面串,比现在更坏。
	if isMSYSBinary(argv0) {
		parts := make([]string, 0, len(argv)+1)
		parts = append(parts, quoteMSYSArg(argv0))
		for _, a := range argv {
			parts = append(parts, quoteMSYSArg(a))
		}
		cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: strings.Join(parts, " ")}
	}
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

// Close 释放会话持有的资源(② 层的 pwsh 引擎、fd 表里的句柄)。
//
// daemon 退出时必须调用:否则每重启一次 daemon 就留下一个常驻 pwsh 进程;
// `exec 4>f` 这类 fd 表条目同理 —— 每个会话漏一个 *os.File。
func (s *Session) Close() error {
	// fd 表(fd 3+)里的句柄:见 third_party/mvdan-sh/WINBASH-PATCH.md §5。
	s.runner.CloseExtraFds()
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

// SetStdinBytes 让紧随其后的那次 Run 从 b 读取 stdin(b 为空即立即 EOF)。
//
// **为什么必须交出管道读端,而不是把 b 包成 bytes.Reader 塞回去**:
// mvdan/sh 的 interp.StdIO 对非 *os.File 的 reader,会在**构造期**就 os.Pipe()
// 并起一支 goroutine 一次性抽干(interp/stdin_os.go 的 newStdinFile)。而 Session
// 是常驻的、StdIO 是构造期 RunnerOption —— 那支 goroutine 早就读到 EOF 并关掉写端了,
// 之后再往 reader 里填什么内容都进不去。后果是**静默失效**:命令拿到空输入、退出码 0,
// 调用方无从分辨"输入是空的"和"输入被丢了" —— 正是这个功能本来要修的 bug。
//
// *os.File 走 newStdinFile 的 `case *os.File` 直通,跳过那次抽干,所以这里用管道。
// (全仓没有任何 runner.Reset() 调用,它会把 stdin 还原成构造期那份 —— 已确认不受影响。)
//
// 写端放进 goroutine 与 Run 并发:os.Pipe 的内核 buffer 默认 64KB,同步写会让
// 大输入把自己卡死(TestStdinPipeLargeInput 钉着这条)。
//
// 读端由 Run 结束时关闭(见 Run 里的 defer),调用方不必管 —— 每请求关一次,
// 句柄才不会在 daemon 长驻期间累积(实测 N=300 每次关 delta=-26,不关 +18)。
func (s *Session) SetStdinBytes(b []byte) {
	pw, err := s.newStdinPipe()
	if err != nil {
		// 建管道失败是系统级故障。退化成空输入:命令立刻 EOF,而不是把调用方
		// 挂在一条命令上等 300s 超时 —— 空输入至少是确定的、可诊断的。
		return
	}
	go func() {
		_, _ = pw.Write(b)
		_ = pw.Close()
	}()
}

// StdinPipe 让紧随其后的那次 Run 从一条管道读取 stdin,并把**写端**交给调用方
// 自己喂(写完必须 Close)。这是**流式**入口 —— daemon 用它把连接上的字节转给脚本。
//
// 与 SetStdinBytes 的分工:
//
//	一次性(SetStdinBytes):Session 内部起 goroutine 写完并 Close,调用方不用管。
//	流式  (StdinPipe)     :数据由调用方从连接读出后写入,Session 根本不知道数据从哪来。
//
// 为什么让调用方持有写端而不是把 reader 塞进 Session:Session 是 shell 层,
// 不该知道 TCP/连接的存在;写端的生命周期与连接的生命周期本来就在 daemon 一侧,
// 归它管分层才干净(见 daemon.handleConn 里 job 的停止时序)。
//
// 两者互斥 —— 一次 Run 只能设其一。两者都在 Run 之前、由同一线程调用:每个会话
// 一个 worker(daemon.serveSession),同一个 Session 上的 Run 在那条队列里严格 FIFO、
// 不并发 —— 这正是这里不做额外加锁的依据。跨会话是并行的,但那时各自 Session 不同。
//
// 读端仍由 Run 结束时的 closeStdin 关闭并换回 /dev/null,调用方不必管读端。
func (s *Session) StdinPipe() (io.WriteCloser, error) {
	return s.newStdinPipe()
}

// newStdinPipe 关掉上一根管道,新建一根,把读端装进 runner、写端托管给 s.stdinW,
// 并把写端返回给调用方。两条 stdin 入口的**唯一**实现,不要各写一份 ——
// 各写一份必然出现「一条路径换了 /dev/null、另一条没换」的分叉,而那正是
// `file already closed` 的来源。
//
// 读端必须是 *os.File:interp.StdIO 对非 *os.File 的 reader 会在**构造期**就
// os.Pipe 并起 goroutine 一次性抽干(理由见 SetStdinBytes 的注释)。
func (s *Session) newStdinPipe() (*os.File, error) {
	// 旧 fd(上一根管道,或 closeStdin 换上的 /dev/null)必须关掉,否则每设一次漏一个。
	s.releaseStdin()

	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	if err := interp.StdIO(pr, s.stdout, s.stderr)(s.runner); err != nil {
		// StdIO 重应用只在 option 非法时报错;pr 是 *os.File 属合法值,所以真走到
		// 这里只能是 mvdan/sh 的行为变了。两端都关掉,别留下半接的管道。
		_ = pr.Close()
		_ = pw.Close()
		return nil, err
	}
	s.stdin = pr
	s.stdinW = pw
	return pw, nil
}

// releaseStdin 关掉当前 stdin 管道的两端并清空字段。幂等。
func (s *Session) releaseStdin() {
	if s.stdin != nil {
		_ = s.stdin.Close()
		s.stdin = nil
	}
	if s.stdinW != nil {
		_ = s.stdinW.Close()
		s.stdinW = nil
	}
}

// closeStdin 关掉当前 stdin 的 fd,并把 runner 的 stdin 换成 /dev/null。
//
// **换成 /dev/null 这一步不能省**:mvdan/sh 的 runner 内部**持有**上一次 StdIO
// 设进去的那个 fd,只关不换的话,下一条不带 stdin 的命令会读到「file already
// closed」—— 实测正是如此:第 1 条命令带 stdin、第 4 条不带,第 4 条就炸了,
// 而且是静默攒到那一刻才炸,与「不静默做错事」完全相反。
//
// /dev/null 是 *os.File,读它立即 EOF,且走 newStdinFile 的 `case *os.File` 直通、
// 不会被抽干 —— 与构造期那个空 reader 的语义一致,但 fd 是干净可控的。
//
// 由 Run 的 defer 调用,所以「这次 Run 有没有用 stdin」都以回到 /dev/null 收尾。
// 读端与写端(releaseStdin)在此一并关掉,两条入口共用这一份收尾逻辑。
func (s *Session) closeStdin() {
	s.releaseStdin()
	f, err := os.Open(os.DevNull)
	if err != nil {
		// 开不出 NUL 极罕见(被占用/权限)。此时保持上一个 fd:它多半已关闭,
		// 命令会**响亮**报错,好过静默返回空输入让人以为脚本没读 stdin。
		return
	}
	if err := interp.StdIO(f, s.stdout, s.stderr)(s.runner); err != nil {
		_ = f.Close()
		return
	}
	s.stdin = f
}

// Run 执行一段脚本,返回 stdout、退出码和错误行。
//
// 退出码沿用 shell 惯例:0 成功,非零失败;语法解析失败返回 2。
// jsonOut 是本次调用的 --json 开关,② 层命令据此改用类型化 JSON 输出。
func (s *Session) Run(ctx context.Context, script string, jsonOut bool) (stdout string, exit int, errs []string) {
	return s.RunArgs(ctx, script, nil, jsonOut)
}

// RunArgs 同 Run,并把 args 绑成位置参数:args[0] 是 $0,args[1:] 依次是 $1/$2/…。
// 与脚本文件路径的绑法一致 —— bash 跑 `./s.sh a b` 与 mvdan 的 DefaultExecHandler
// 跑脚本文件(interp/handler.go)都是「文件名当 $0、其余当 $1+」,`bash -c 's' name a`
// 也是同一形状。args 为空时与 Run 完全等价(解析 name 用空串,Params 一字不动)。
//
// 会话级状态的两条纪律:
//   - $1+ 落在 runner.Params(interp/vars.go 的 "1".."9" 分支),它是**跨 Run 持久**
//     的会话状态,注入后必须还原 —— 否则这条命令的 $1 会泄漏给同一会话的下一条命令。
//   - $0 走的是 File.Name(interp/api.go 的 Run:filename 每次 Run 都重置),不持久,
//     无需还原。
func (s *Session) RunArgs(ctx context.Context, script string, args []string, jsonOut bool) (stdout string, exit int, errs []string) {
	name := ""
	if len(args) > 0 {
		name = args[0]
	}

	s.jsonOut = jsonOut

	s.outBuf.Reset()
	s.errBuf.Reset()
	s.stdout.Set(&s.outBuf)
	s.stderr.Set(&s.errBuf)
	defer func() {
		s.stdout.Set(nil)
		s.stderr.Set(nil)
		// stdin 只属于这一次 Run:关掉读端,句柄才不会在 daemon 长驻期间累积。
		s.closeStdin()
	}()

	// 复用会话的 parser(见 Session.parser 的注释):mvdan 文档明确 Parse 可反复
	// 调用、开头必 reset(),每次传各自的 name,所以逐条命令复用行为零变化。
	prog, perr := s.parser.Parse(strings.NewReader(script), name)
	if perr != nil {
		return "", 2, []string{perr.Error()}
	}
	// 解析之后、运行之前:拦住 Windows 上会静默降级的语法(进程替换、未实现的参数展开)。
	// 一遍遍历同时做两个检查并收集「有没有后台任务」—— 报错优先级与原先两遍遍历一致:
	// 先进程替换、后锚定展开;后台任务只是收集性信息,与这两个报错无关。
	hasBg, rerr := rejectUnsupportedSyntax(prog)
	if rerr != nil {
		return "", 2, []string{rerr.Error()}
	}

	// 注入 $1+。直接读写导出字段而不是 interp.Params(...) 选项:那个选项按 `set`
	// 的规则解析开头带 - 的参数(`Params("-e", …)` 会打开 errexit 而不是把它绑成 $0),
	// 而这里绑的是**用户写的原样参数**,不解析。保存/还原的形状与 interp 自己对
	// 函数调用的处理一致(interp/runner.go 的 call:oldParams → r.Params = args[1:]
	// → 执行 → 还原)。Restore 恒为旧值,即使脚本里跑过 `set -- x y` 也一样:
	// 这次请求的参数来自调用方 argv,不该被会话记住。
	if len(args) > 1 {
		oldParams := s.runner.Params
		s.runner.Params = args[1:]
		defer func() { s.runner.Params = oldParams }()
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
	// 后台任务警告(2026-10-31 会话语义决议):`&` 的行为不变 —— 后台 job 仍随本次
	// 请求结束被取消(差异表已记账、e2e 已钉),但脚本里**有**后台任务时追加一行指路
	// `--detach` 的警告,把「静默数据丢失」变成「看得见的取舍」。走 errs(即响应的
	// Errors 通道,与 daemon 的 note、stdin 源端读错误同一先例)而不改退出码 ——
	// 对照组「脚本存活期间的后台 job 正常」是真实合法用法,不能因为警告而失败。
	// 整个脚本只警告一次:hasBg 是布尔,`a & b & c` 也只追加一行。
	if hasBg {
		errs = append(errs, backgroundJobWarning)
	}
	return s.outBuf.String(), exit, errs
}

// backgroundJobWarning 是脚本含后台任务时的固定一行。措辞即决议原文:行为不变 +
// 指路常驻方案(`--detach`)。它进响应的 Errors 通道(非致命提示),不是错误。
const backgroundJobWarning = "警告: `cmd &` 的后台任务会随本次请求结束被取消(输出与副作用一起消失);" +
	"要脱离会话常驻,用 winbash --detach -c '…'"

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
