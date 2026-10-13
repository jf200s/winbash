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

// NewSession 建立一个会话,初始工作目录为当前进程的目录。
func NewSession() *Session {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}

	s := &Session{
		stdout: &switchWriter{},
		stderr: &switchWriter{},
	}

	runner, err := interp.New(
		interp.Dir(cwd),
		interp.Env(expand.ListEnviron(os.Environ()...)),
		interp.StdIO(nil, s.stdout, s.stderr),
		interp.ExecHandler(s.execHandler),
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

	switch classify(args[0]) {
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
		if code := winmgmt.Registry[args[0]](ctx, conn, args[1:], s.jsonOut, hc.Stdout, hc.Stderr); code != 0 {
			return interp.ExitStatus(code)
		}
		return nil
	}

	// ③ 层:暂由 mvdan/sh 的默认 handler 兜着(走进程 PATH)。
	// Task 12 会换成自定义 handler —— 默认 handler 只认进程 PATH,而我们要能加上
	// Git Bash 的 usr\bin,同时不改环境变量(Global Constraints)。
	return interp.DefaultExecHandler(0)(ctx, args)
}

// psEngine 返回 ② 层的常驻 pwsh,第一次真的用到时才启动。
//
// 惰性是有意的:只跑 ① 层的会话(例如 `ls | grep x`)不该付那次 ~350ms 的启动成本。
// 失败结果同样缓存 —— 否则 `svc list; svc status x` 会为同一个错误各等一次超时。
func (s *Session) psEngine() (*pwsh.Conn, error) {
	if s.engine != nil {
		return s.engine, nil
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
	if runErr != nil && exit != 0 {
		errs = append(errs, runErr.Error())
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
