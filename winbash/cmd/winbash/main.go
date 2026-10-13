// Command winbash 是一个 bash 语法 + pwsh 能力的常驻 shell。
//
// 它让 AI 不必在"我熟悉的语法"和"够用的能力"之间做取舍 —— 这正是当前在 Windows 上
// 翻车的根源:bash 熟但管不了 Windows,pwsh 管得了但不熟且慢。
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"winbash/internal/client"
	"winbash/internal/daemon"
)

func main() { os.Exit(run()) }

func run() int {
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

	flag.StringVar(&script, "c", "", "要执行的脚本")
	flag.StringVar(&script, "script", "", "要执行的脚本(同 -c)")
	flag.BoolVar(&jsonOut, "json", false, "输出类型化 JSON")
	flag.BoolVar(&jsonOut, "j", false, "输出类型化 JSON(同 --json)")
	flag.BoolVar(&serve, "serve", false, "前台运行 daemon")
	flag.BoolVar(&stop, "stop", false, "停止 daemon")
	flag.BoolVar(&status, "status", false, "打印 daemon 状态")
	flag.StringVar(&endpoint, "endpoint", defaultEndpoint(), "daemon 的端口文件路径")
	flag.DurationVar(&idle, "idle", 30*time.Minute, "空闲多久后 daemon 自行退出(0 = 永不)")
	flag.DurationVar(&timeout, "timeout", 300*time.Second, "单次请求的超时(0 = 不设限)")
	flag.Parse()

	if serve {
		if err := daemon.Serve(endpoint, idle); err != nil {
			fmt.Fprintln(os.Stderr, "winbash: serve:", err)
			return 1
		}
		return 0
	}

	// --stop / --status 都不会把 daemon 拉起来:为了停掉它而先启动它、
	// 或者为了看状态而启动一个,都不合理(engine 那边踩过同类的坑)。
	if stop {
		if err := client.Stop(endpoint); err != nil {
			fmt.Fprintln(os.Stderr, "winbash:", err)
			return 1
		}
		return 0
	}
	if status {
		out, err := client.Status(endpoint)
		if err != nil {
			fmt.Fprintln(os.Stderr, "winbash:", err)
			return 1
		}
		fmt.Print(out)
		return 0
	}

	if script == "" {
		script = strings.Join(flag.Args(), " ")
	}
	if strings.TrimSpace(script) == "" {
		fmt.Fprintln(os.Stderr, "winbash: 没有要执行的脚本。用法: winbash -c 'ls -l'")
		return 2
	}

	cwd, err := os.Getwd()
	if err != nil {
		cwd = ""
	}

	resp, err := client.RunTimeout(endpoint, script, cwd, jsonOut, timeout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "winbash:", err)
		return 1
	}

	// 直接写字节,不走 string —— 保证二进制输出不被改写。
	if len(resp.Out) > 0 {
		_, _ = os.Stdout.Write(resp.Out)
	}
	for _, e := range resp.Errors {
		fmt.Fprintln(os.Stderr, e)
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
