// Command pwshx runs PowerShell through a persistent daemon.
//
// Starting pwsh costs ~350ms. pwshx keeps one pwsh process alive and talks to
// it over a named pipe: a round trip is ~1ms, and a whole invocation ~17ms
// (Windows process creation dominates). Because it is a single session,
// variables and the working directory survive between invocations, which plain
// pwsh cannot do.
//
// Protocol (both directions): 4-byte little-endian length, then UTF-8 JSON.
package main

import (
	_ "embed"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
)

//go:embed server.ps1
var serverScript string

const (
	createNoWindow   = 0x08000000
	createNewProcGrp = 0x00000200
	connectBudget    = 20 * time.Second
	maxResponseBytes = 64 << 20
)

type request struct {
	Op     string `json:"op"`
	Script string `json:"script,omitempty"`
	Cwd    string `json:"cwd,omitempty"`
	JSON   bool   `json:"json,omitempty"`
}

type meta struct {
	Pid   int   `json:"pid"`
	Calls int   `json:"calls"`
	Ms    int64 `json:"ms"`
}

type response struct {
	OK     bool     `json:"ok"`
	Out    string   `json:"out"`
	Errors []string `json:"errors"`
	Meta   meta     `json:"meta"`
}

type client struct {
	pipeBase string // pipe name without the \\.\pipe\ prefix
	idleMs   int
	timeout  time.Duration
	quiet    bool
}

// pipePath is what a client connects to. The daemon itself only ever sees the
// bare name, because NamedPipeServerStream rejects the \\.\pipe\ form.
func (c *client) pipePath() string { return `\\.\pipe\` + c.pipeBase }

func main() { os.Exit(run()) }

func run() int {
	var (
		script    string
		file      string
		jsonOut   bool
		stop      bool
		status    bool
		serve     bool
		cwd       string
		noCwdSync bool
		timeoutS  int
		idleMin   int
		quiet     bool
	)

	flag.StringVar(&script, "c", "", "script to execute")
	flag.StringVar(&script, "script", "", "script to execute")
	flag.StringVar(&file, "f", "", "read script from file")
	flag.StringVar(&file, "file", "", "read script from file")
	flag.BoolVar(&jsonOut, "json", false, "emit JSON instead of formatted text")
	flag.BoolVar(&jsonOut, "j", false, "emit JSON instead of formatted text")
	flag.BoolVar(&stop, "stop", false, "stop the daemon")
	flag.BoolVar(&status, "status", false, "show daemon status")
	flag.BoolVar(&serve, "serve", false, "run the daemon in the foreground (debugging)")
	flag.StringVar(&cwd, "cwd", "", "directory to run in (default: this process's directory)")
	flag.BoolVar(&noCwdSync, "no-cwd-sync", false, "leave the daemon's own working directory alone")
	flag.IntVar(&timeoutS, "timeout", 300, "request timeout in seconds (0 = wait forever)")
	flag.IntVar(&idleMin, "idle", 30, "shut the daemon down after N idle minutes (0 = never)")
	flag.BoolVar(&quiet, "q", false, "suppress the daemon startup notice")
	flag.BoolVar(&quiet, "quiet", false, "suppress the daemon startup notice")

	flag.Usage = usage
	flag.Parse()

	c := &client{
		pipeBase: pipeName(),
		idleMs:   idleMin * 60 * 1000,
		timeout:  time.Duration(timeoutS) * time.Second,
		quiet:    quiet,
	}

	if serve {
		return c.serveForeground()
	}
	if stop {
		return c.oneShot(request{Op: "stop"}, true)
	}
	if status {
		return c.oneShot(request{Op: "ping"}, true)
	}

	if script == "" && file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			fmt.Fprintf(os.Stderr, "pwshx: %v\n", err)
			return 1
		}
		script = string(b)
	}
	if script == "" && flag.NArg() > 0 {
		script = strings.Join(flag.Args(), " ")
	}
	if script == "" && stdinHasData() {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintf(os.Stderr, "pwshx: reading stdin: %v\n", err)
			return 1
		}
		script = string(b)
	}

	if strings.TrimSpace(script) == "" {
		usage()
		return 2
	}

	req := request{Op: "run", Script: script, JSON: jsonOut}
	if !noCwdSync {
		req.Cwd = cwd
		if req.Cwd == "" {
			if wd, err := os.Getwd(); err == nil {
				req.Cwd = wd
			}
		}
	}
	return c.oneShot(req, false)
}

// oneShot connects (starting the daemon if allowed) and performs one request.
// mustExist keeps --stop/--status from bringing a daemon to life just to kill
// or describe it.
func (c *client) oneShot(req request, mustExist bool) int {
	f, started, err := c.connect(mustExist)
	if err != nil {
		if mustExist {
			fmt.Fprintln(os.Stderr, "pwshx: daemon is not running")
			return 0
		}
		fmt.Fprintf(os.Stderr, "pwshx: %v\n", err)
		return 1
	}
	defer f.Close()

	if started && !c.quiet {
		fmt.Fprintln(os.Stderr, "pwshx: started daemon (later calls skip the ~350ms pwsh startup)")
	}

	resp, err := c.call(f, req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pwshx: %v\n", err)
		return 1
	}

	out := strings.TrimRight(resp.Out, "\r\n\t ")
	if out != "" {
		fmt.Fprintln(os.Stdout, out)
	}
	for _, e := range resp.Errors {
		fmt.Fprintln(os.Stderr, "pwshx: "+e)
	}
	if !resp.OK {
		return 1
	}
	return 0
}

func (c *client) connect(mustExist bool) (*os.File, bool, error) {
	f, err := os.OpenFile(c.pipePath(), os.O_RDWR, 0)
	if err == nil {
		return f, false, nil
	}

	// ERROR_PIPE_BUSY means another invocation is mid-request. Wait for it
	// instead of spawning a second daemon, which would only fail to bind and
	// burn a ~350ms pwsh start for nothing.
	busy := isPipeBusy(err)
	if mustExist && !busy {
		return nil, false, fmt.Errorf("daemon is not running")
	}

	started := false
	if !busy {
		if err := c.startDaemon(); err != nil {
			return nil, false, fmt.Errorf("starting daemon: %w", err)
		}
		started = true
	}

	budget := connectBudget
	if busy {
		budget = c.timeout
		if budget <= 0 {
			budget = 24 * time.Hour
		}
	}

	deadline := time.Now().Add(budget)
	var lastErr error
	for time.Now().Before(deadline) {
		time.Sleep(15 * time.Millisecond)
		f, err := os.OpenFile(c.pipePath(), os.O_RDWR, 0)
		if err == nil {
			return f, started, nil
		}
		lastErr = err
	}
	return nil, false, fmt.Errorf("daemon did not answer within %s (last error: %v)", budget, lastErr)
}

// errorPipeBusy is Windows' ERROR_PIPE_BUSY, which Go's syscall package does
// not export.
const errorPipeBusy = syscall.Errno(231)

func isPipeBusy(err error) bool {
	var errno syscall.Errno
	return errors.As(err, &errno) && errno == errorPipeBusy
}

func (c *client) startDaemon() error {
	encoded := base64.StdEncoding.EncodeToString(utf16LE(serverScript))
	cmd := exec.Command(findPwsh(),
		"-NoProfile", "-NoLogo", "-NonInteractive",
		"-ExecutionPolicy", "Bypass",
		"-EncodedCommand", encoded,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow | createNewProcGrp,
	}
	cmd.Env = append(os.Environ(),
		"PWSHX_PIPE="+c.pipeBase,
		fmt.Sprintf("PWSHX_IDLE=%d", c.idleMs),
	)
	// The daemon has no console, so without this a failure to start is silent.
	if logPath := os.Getenv("PWSHX_LOG"); logPath != "" {
		if lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			cmd.Stdout, cmd.Stderr = lf, lf
		}
	}
	return cmd.Start()
}

func (c *client) serveForeground() int {
	cmd := exec.Command(findPwsh(),
		"-NoProfile", "-NoLogo", "-ExecutionPolicy", "Bypass",
		"-EncodedCommand", base64.StdEncoding.EncodeToString(utf16LE(serverScript)),
	)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(),
		"PWSHX_PIPE="+c.pipeBase,
		fmt.Sprintf("PWSHX_IDLE=%d", c.idleMs),
	)
	fmt.Fprintf(os.Stderr, "pwshx: serving on %s\n", c.pipePath())
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "pwshx: %v\n", err)
		return 1
	}
	return 0
}

func (c *client) call(f *os.File, req request) (response, error) {
	if c.timeout <= 0 {
		return exchange(f, req)
	}
	type result struct {
		resp response
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		resp, err := exchange(f, req)
		ch <- result{resp, err}
	}()
	select {
	case r := <-ch:
		return r.resp, r.err
	case <-time.After(c.timeout):
		return response{}, fmt.Errorf("no reply within %s", c.timeout)
	}
}

func exchange(f *os.File, req request) (response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return response{}, err
	}
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], uint32(len(body)))
	if _, err := f.Write(hdr[:]); err != nil {
		return response{}, fmt.Errorf("sending request: %w", err)
	}
	if _, err := f.Write(body); err != nil {
		return response{}, fmt.Errorf("sending request: %w", err)
	}
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return response{}, fmt.Errorf("reading reply: %w", err)
	}
	n := binary.LittleEndian.Uint32(hdr[:])
	if n == 0 || n > maxResponseBytes {
		return response{}, fmt.Errorf("implausible reply length %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(f, buf); err != nil {
		return response{}, fmt.Errorf("reading reply: %w", err)
	}
	var resp response
	if err := json.Unmarshal(buf, &resp); err != nil {
		return response{}, fmt.Errorf("malformed reply from daemon: %w", err)
	}
	return resp, nil
}

func pipeName() string {
	user := os.Getenv("USERNAME")
	if user == "" {
		user = "default"
	}
	var b strings.Builder
	for _, r := range user {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return "pwshx-" + b.String()
}

func findPwsh() string {
	if p, err := exec.LookPath("pwsh.exe"); err == nil {
		return p
	}
	candidates := []string{
		filepath.Join(os.Getenv("ProgramFiles"), "PowerShell", "7", "pwsh.exe"),
		filepath.Join(os.Getenv("ProgramFiles"), "PowerShell", "7-preview", "pwsh.exe"),
		filepath.Join(os.Getenv("LOCALAPPDATA"), "Microsoft", "WindowsApps", "pwsh.exe"),
	}
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return "pwsh.exe"
}

func utf16LE(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := make([]byte, len(u)*2)
	for i, r := range u {
		binary.LittleEndian.PutUint16(b[i*2:], r)
	}
	return b
}

func stdinHasData() bool {
	st, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice == 0
}

func usage() {
	fmt.Fprint(os.Stderr, `pwshx — run PowerShell without paying its ~350ms startup cost.

Usage:
  pwshx [flags] <script>
  pwshx -c <script>          explicit script
  pwshx -f script.ps1        read script from a file
  echo '<script>' | pwshx    read script from stdin
  pwshx --stop | --status    manage the daemon

Flags:
  -c, --script <s>   script to execute
  -f, --file <path>  read the script from a file
  -j, --json         emit machine-readable JSON instead of formatted text
      --cwd <path>   directory to run in (default: this process's directory)
      --no-cwd-sync  leave the daemon's own working directory alone
      --timeout <s>  request timeout in seconds (default 300, 0 = forever)
      --idle <min>   daemon exits after N idle minutes (default 30, 0 = never)
      --stop         stop the daemon
      --status       print daemon status
      --serve        run the daemon in the foreground (debugging)
  -q, --quiet        suppress the daemon startup notice

The daemon is a single long-lived pwsh session, so variables and the current
directory persist between invocations.
`)
}
