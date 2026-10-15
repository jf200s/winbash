// Command winbash 是一个 bash 语法 + pwsh 能力的常驻 shell。
//
// 它让 AI 不必在"我熟悉的语法"和"够用的能力"之间做取舍 —— 这正是当前在 Windows 上
// 翻车的根源:bash 熟但管不了 Windows,pwsh 管得了但不熟且慢。
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"winbash/internal/client"
	"winbash/internal/daemon"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

// run 是 main 的全部逻辑,参数与输出都可注入 —— 这样测试能直接驱动它。
//
// 早先它读**全局** flag.CommandLine、只写 os.Stdout/os.Stderr,于是 cmd/winbash 的
// 覆盖率是 0%(审计报告点过这一条):既不能重复调用(第二次注册同名 flag 会 panic),
// 也没法断言它往哪儿写了什么。换成局部 FlagSet 之后两件事都成立。
//
// 不把 main 本身纳入覆盖:那要构建真 exe 再 os/exec 跑,而子进程的覆盖率不会回传给
// go test(除非 go build -cover + GOCOVERDIR 那一整套),收益不抵复杂度。它只剩一行。
func run(args []string, stdout, stderr io.Writer) int {
	var (
		script   string
		jsonOut  bool
		serve    bool
		stop     bool
		status   bool
		endpoint string
		idle     time.Duration
		timeout  time.Duration
	)

	fs := flag.NewFlagSet("winbash", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&script, "c", "", "要执行的脚本")
	fs.StringVar(&script, "script", "", "要执行的脚本(同 -c)")
	fs.BoolVar(&jsonOut, "json", false, "输出类型化 JSON")
	fs.BoolVar(&jsonOut, "j", false, "输出类型化 JSON(同 --json)")
	fs.BoolVar(&serve, "serve", false, "前台运行 daemon")
	fs.BoolVar(&stop, "stop", false, "停止 daemon")
	fs.BoolVar(&status, "status", false, "打印 daemon 状态")
	fs.StringVar(&endpoint, "endpoint", defaultEndpoint(), "daemon 的端口文件路径")
	fs.DurationVar(&idle, "idle", 30*time.Minute, "空闲多久后 daemon 自行退出(0 = 永不);自动拉起 daemon 时也会转给它")
	fs.DurationVar(&timeout, "timeout", 300*time.Second, "单次请求的超时(0 = 不设限)")
	if err := fs.Parse(args); err != nil {
		// 不认识的 flag 一律非零退出(项目底线)。用法已经由 FlagSet 打到 stderr。
		return 2
	}

	if serve {
		if err := daemon.Serve(endpoint, idle, daemon.DefaultReadTimeout); err != nil {
			fmt.Fprintln(stderr, "winbash: serve:", err)
			return 1
		}
		return 0
	}

	// --stop / --status 都不会把 daemon 拉起来:为了停掉它而先启动它、
	// 或者为了看状态而启动一个,都不合理(engine 那边踩过同类的坑)。
	if stop {
		if err := client.Stop(endpoint); err != nil {
			fmt.Fprintln(stderr, "winbash:", err)
			return 1
		}
		return 0
	}
	if status {
		out, err := client.Status(endpoint)
		if err != nil {
			fmt.Fprintln(stderr, "winbash:", err)
			return 1
		}
		fmt.Fprint(stdout, out)
		return 0
	}

	if script == "" {
		script = strings.Join(fs.Args(), " ")
	}
	if strings.TrimSpace(script) == "" {
		fmt.Fprintln(stderr, "winbash: 没有要执行的脚本。用法: winbash -c 'ls -l'")
		return 2
	}

	cwd, err := os.Getwd()
	if err != nil {
		cwd = ""
	}

	resp, err := client.RunTimeout(endpoint, script, cwd, jsonOut, timeout, idle)
	if err != nil {
		fmt.Fprintln(stderr, "winbash:", err)
		return 1
	}

	// 直接写字节,不走 string —— 保证二进制输出不被改写。
	if len(resp.Out) > 0 {
		_, _ = stdout.Write(resp.Out)
	}
	for _, e := range resp.Errors {
		fmt.Fprintln(stderr, e)
	}
	return resp.Exit
}

// defaultEndpoint 是端口文件的路径。按用户名区分,避免同机多用户互踩。
func defaultEndpoint() string {
	user := os.Getenv("USERNAME")
	if user == "" {
		user = "default"
	}
	return filepath.Join(os.TempDir(), "winbash-"+sanitize(user)+".port")
}

// sanitize 只保留可安全放进文件名的字符(本机的用户名含空格,例如 "Test User")。
func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "default"
	}
	return b.String()
}
