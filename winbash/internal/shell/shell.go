// Package shell 把 mvdan/sh 的 interp 包装成一个可复用的会话。
//
// 一个 Session 持有 interp.Runner 的全部状态(变量、cwd、函数),因此连续多次 Run
// 之间的 shell 状态是保持的 —— 这正是 AI 在 Windows 上本来缺失的东西。
package shell

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// Session 是一次命令行"会话":它跨多次 Run 保持变量、cwd 与函数。
//
// 不保证并发安全(interp.Runner 本身也不安全)。
type Session struct {
	runner *interp.Runner

	outBuf, errBuf bytes.Buffer
	stdout, stderr *switchWriter
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
	)
	if err != nil {
		// interp.New 只在 RunnerOption 非法时报错;上面的选项都是常量,出错即编程错误。
		panic("shell: interp.New: " + err.Error())
	}
	s.runner = runner
	return s
}

// Dir 返回会话当前的工作目录。cd 之后它会跟着变。
func (s *Session) Dir() string { return s.runner.Dir }

// Run 执行一段脚本,返回 stdout、退出码和错误行。
//
// 退出码沿用 shell 惯例:0 成功,非零失败;语法解析失败返回 2。
func (s *Session) Run(ctx context.Context, script string) (stdout string, exit int, errs []string) {
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
