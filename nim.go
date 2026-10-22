// nim.go: the ./nim front end. service control plus the status/log commands,
// replacing the old sh script and nimstatus.py.
package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const nimSvc = "com.user.nimroute"

const nimUsage = `nim - run and inspect the nimroute proxy

usage: ./nim <command> [args]

  run [flags]        foreground; extra flags go to the binary
  start [flags]      start the background service
  stop               stop the service
  restart [flags]    stop, then start
  status [tail]      service state, then the dashboard
  status-tail        dashboard, refreshing in place
  logs               recent traffic
  log [N|tail]       last N lines, or follow
  tail               follow the access log
  models [N]         model list with benchmark scores
  models sync        refresh the model catalog
  freepi auth        github device login for the freepi backend
  install [flags]    install the service
  uninstall          remove the service
  help               print this

binary flags:  --config PATH  --port N  --lanes N  --debug
               -df  --dbg-file=DIR   dump failed requests as json
config:        config.yml, hot-reloaded. docs/service.md has the details.
`

// nimEnv is the resolved layout the sh script derived from $0 and $HOME.
type nimEnv struct {
	dir     string // repo root: the directory holding the binary
	bin     string // <dir>/nimroute
	logFile string // <dir>/nimroute.log
	pidFile string // <dir>/.nimroute.pid
	plist   string // ~/Library/LaunchAgents/com.user.nimroute.plist
	systemd string // ~/.config/systemd/user/nimroute.service
	port    string
	mode    string // launchd | systemd | nohup
}

func newNimEnv() nimEnv {
	dir := "."
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		dir = filepath.Dir(exe)
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	home, _ := os.UserHomeDir()
	port := os.Getenv("PORT")
	if port == "" {
		port = "5419"
	}
	return nimEnv{
		dir:     dir,
		bin:     filepath.Join(dir, "nimroute"),
		logFile: filepath.Join(dir, "nimroute.log"),
		pidFile: filepath.Join(dir, ".nimroute.pid"),
		plist:   filepath.Join(home, "Library", "LaunchAgents", nimSvc+".plist"),
		systemd: filepath.Join(home, ".config", "systemd", "user", "nimroute.service"),
		port:    port,
		mode:    nimSvcMode(),
	}
}

func nimSvcMode() string {
	switch runtime.GOOS {
	case "darwin":
		return "launchd"
	case "linux":
		if _, err := exec.LookPath("systemctl"); err == nil {
			return "systemd"
		}
	}
	return "nohup"
}

// needBin: every command that launches the proxy needs a build. without this
// the shell reports "no such file or directory", which reads like a broken
// install rather than an unbuilt one.
func (e nimEnv) needBin() bool {
	if _, err := os.Stat(e.bin); err == nil {
		return true
	}
	fmt.Fprintf(os.Stderr, "nim: no nimroute binary at %s\n", e.bin)
	if _, err := exec.LookPath("go"); err == nil {
		fmt.Fprintln(os.Stderr, "     build it first:  go build -o nimroute .")
	} else {
		fmt.Fprintln(os.Stderr, "     go is not installed (needs Go 1.26+): https://go.dev/dl/")
		fmt.Fprintln(os.Stderr, "     macOS: brew install go   |   Linux: sudo apt install golang-go")
	}
	return false
}

// nimFindPy is only needed by `freepi auth` now; the renderer is go.
func nimFindPy() (string, bool) {
	for _, p := range []string{"/opt/homebrew/bin/python3", "/usr/local/bin/python3", "python3"} {
		path := p
		if strings.Contains(p, "/") {
			if _, err := os.Stat(p); err != nil {
				continue
			}
		} else if found, err := exec.LookPath(p); err == nil {
			path = found
		} else {
			continue
		}
		if exec.Command(path, "-c", "pass").Run() == nil {
			return path, true
		}
	}
	return "", false
}

func (e nimEnv) needPy(what string) string {
	if py, ok := nimFindPy(); ok {
		return py
	}
	fmt.Fprintf(os.Stderr, "nim: python3 not found, and '%s' needs it\n", what)
	fmt.Fprintln(os.Stderr, "     install python3 (macOS: brew install python3; debian: apt install python3)")
	fmt.Fprintf(os.Stderr, "     raw status is still available:  curl -s http://localhost:%s/status\n", e.port)
	os.Exit(1)
	return ""
}

func (e nimEnv) running() bool {
	switch e.mode {
	case "launchd":
		out, err := exec.Command("launchctl", "list", nimSvc).Output()
		if err != nil {
			return false
		}
		for _, ln := range strings.Split(string(out), "\n") {
			if strings.Contains(ln, `"PID"`) && !strings.Contains(ln, "= 0") {
				return true
			}
		}
		return false
	case "systemd":
		return exec.Command("systemctl", "--user", "is-active", "-q", "nimroute").Run() == nil
	default:
		b, err := os.ReadFile(e.pidFile)
		if err != nil {
			return false
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
		if err != nil || pid <= 0 {
			return false
		}
		p, err := os.FindProcess(pid)
		if err != nil {
			return false
		}
		return p.Signal(syscall.Signal(0)) == nil
	}
}

// portAlive reports whether something is serving on the proxy port. catches
// instances started manually so status reads correctly and start does not
// double-bind.
func (e nimEnv) portAlive() bool {
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", e.port), 2*time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func nimUID() (string, bool) {
	out, err := exec.Command("id", "-u").Output()
	if err != nil {
		return "", false
	}
	uid := strings.TrimSpace(string(out))
	return uid, uid != ""
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// start passes any extra args through to the binary: both templates carry
// REPLACE_WITH_ARGS, so `nim restart -df` starts a recording service without
// editing the templates by hand.
func (e nimEnv) start(args []string) {
	switch e.mode {
	case "launchd":
		e.startLaunchd(args)
	case "systemd":
		e.startSystemd(args)
	default:
		e.startNohup(args)
	}
}

func (e nimEnv) startLaunchd(args []string) {
	tmpl, err := os.ReadFile(filepath.Join(e.dir, "deploy", nimSvc+".plist"))
	if err != nil {
		nimFatal("cannot read the launchd template", err)
	}
	// the placeholder sits right after </string>, so every arg carries its own
	// leading newline and indentation.
	var extra strings.Builder
	for _, a := range args {
		fmt.Fprintf(&extra, "\n    <string>%s</string>", xmlEscape(a))
	}
	body := strings.ReplaceAll(string(tmpl), "REPLACE_WITH_PATH", e.dir)
	body = strings.Replace(body, "REPLACE_WITH_ARGS", extra.String(), 1)
	if err := os.MkdirAll(filepath.Dir(e.plist), 0o755); err != nil {
		nimFatal("cannot create the LaunchAgents directory", err)
	}
	if err := os.WriteFile(e.plist, []byte(body), 0o644); err != nil {
		nimFatal("cannot write "+e.plist, err)
	}
	uid, ok := nimUID()
	if !ok {
		nimFatal("cannot read the current uid", nil)
	}
	_ = exec.Command("launchctl", "bootout", "gui/"+uid+"/"+nimSvc).Run()
	if err := exec.Command("launchctl", "bootstrap", "gui/"+uid, e.plist).Run(); err != nil {
		_ = exec.Command("launchctl", "load", e.plist).Run()
	}
}

func (e nimEnv) startSystemd(args []string) {
	tmpl, err := os.ReadFile(filepath.Join(e.dir, "deploy", "nimroute.service"))
	if err != nil {
		nimFatal("cannot read the systemd template", err)
	}
	var extra strings.Builder
	for _, a := range args {
		extra.WriteString(" " + a)
	}
	body := strings.ReplaceAll(string(tmpl), "REPLACE_WITH_PATH", e.dir)
	body = strings.Replace(body, "REPLACE_WITH_ARGS", extra.String(), 1)
	if err := os.MkdirAll(filepath.Dir(e.systemd), 0o755); err != nil {
		nimFatal("cannot create the systemd user directory", err)
	}
	if err := os.WriteFile(e.systemd, []byte(body), 0o644); err != nil {
		nimFatal("cannot write "+e.systemd, err)
	}
	if exec.Command("systemctl", "--user", "daemon-reload").Run() == nil {
		_ = exec.Command("systemctl", "--user", "enable", "--now", "nimroute").Run()
	}
}

func (e nimEnv) startNohup(args []string) {
	f, err := os.OpenFile(e.logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		nimFatal("cannot open "+e.logFile, err)
	}
	cmd := exec.Command(e.bin, args...)
	cmd.Dir = e.dir
	cmd.Stdout = f
	cmd.Stderr = f
	if err := cmd.Start(); err != nil {
		nimFatal("cannot start "+e.bin, err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	f.Close()
	_ = os.WriteFile(e.pidFile, []byte(strconv.Itoa(pid)+"\n"), 0o600)
}

func (e nimEnv) stop() {
	switch e.mode {
	case "launchd":
		// bootout both labels: pre-rename instances still run as
		// com.user.nvidia-nim-proxy, current as com.user.nimroute.
		uid, ok := nimUID()
		if ok {
			if exec.Command("launchctl", "bootout", "gui/"+uid+"/"+nimSvc).Run() != nil {
				_ = exec.Command("launchctl", "unload", e.plist).Run()
			}
			_ = exec.Command("launchctl", "bootout", "gui/"+uid+"/com.user.nvidia-nim-proxy").Run()
		}
	case "systemd":
		_ = exec.Command("systemctl", "--user", "disable", "--now", "nimroute").Run()
	default:
		if b, err := os.ReadFile(e.pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
				if p, err := os.FindProcess(pid); err == nil {
					_ = p.Signal(syscall.SIGTERM)
				}
			}
			_ = os.Remove(e.pidFile)
		}
	}
}

// prettyStatus prints the state line then the dashboard. stopped exits 1 so
// `./nim status` is usable in a shell condition.
func (e nimEnv) prettyStatus() int {
	if !e.running() && !e.portAlive() {
		fmt.Println("nimroute: STOPPED")
		if _, err := os.Stat(e.bin); err != nil {
			fmt.Println("  not built yet:  go build -o nimroute .")
		}
		return 1
	}
	fmt.Printf("nimroute: RUNNING (%s)\n", e.mode)
	fmt.Println()
	nimPrintStatus(e.port)
	fmt.Println()
	return 0
}

func nimFatal(what string, err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "nim: %s: %v\n", what, err)
	} else {
		fmt.Fprintf(os.Stderr, "nim: %s\n", what)
	}
	os.Exit(1)
}

// nimExec replaces this process with argv, forwarding the exit code. the child
// shares the process group, so the parent must ignore ^C while it runs or nim
// dies first and leaves the proxy mid-shutdown.
func nimExec(argv []string) {
	if len(argv) == 0 {
		return
	}
	signal.Ignore(os.Interrupt, syscall.SIGTERM)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	signal.Reset(os.Interrupt, syscall.SIGTERM)
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			os.Exit(ee.ExitCode())
		}
		nimFatal("cannot run "+argv[0], err)
	}
	os.Exit(0)
}

var proxyCredRE = regexp.MustCompile(`://[^/@\s]+@`)

// tailFile reads the last n lines without loading the whole file. the access
// log grows forever, so a bounded backward seek is the only safe read.
func tailFile(path string, n int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	pos, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	const blk = 8192
	var buf []byte
	nl := 0
	for pos > 0 && nl <= n {
		step := int64(blk)
		if pos < step {
			step = pos
		}
		pos -= step
		chunk := make([]byte, step)
		if _, err := f.ReadAt(chunk, pos); err != nil && err != io.EOF {
			return nil, err
		}
		nl += strings.Count(string(chunk), "\n")
		buf = append(chunk, buf...)
	}
	lines := strings.Split(string(buf), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}

// followLog blocks like tail -f, printing only what lands after the call.
func followLog(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	off, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	buf := make([]byte, 32*1024)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			os.Stdout.Write(buf[:n])
			off += int64(n)
		}
		if err != nil && err != io.EOF {
			return err
		}
		if n == 0 {
			// a rotated or truncated log restarts from the top
			if st, err := os.Stat(path); err == nil && st.Size() < off {
				if _, err := f.Seek(0, io.SeekStart); err == nil {
					off = 0
				}
			}
			time.Sleep(250 * time.Millisecond)
		}
	}
}

// nimName reports whether we were invoked as ./nim, the front end, rather
// than ./nimroute, the server. ./nim is a symlink to the same binary.
func nimName() bool {
	return filepath.Base(os.Args[0]) == "nim"
}

func runNim(args []string) {
	e := newNimEnv()
	cmd := "status"
	if len(args) > 0 {
		cmd = args[0]
	}
	arg := func(i int) string {
		if len(args) > i {
			return args[i]
		}
		return ""
	}

	switch cmd {
	case "run":
		if !e.needBin() {
			os.Exit(1)
		}
		nimExec(append([]string{e.bin}, args[1:]...))
	case "start":
		if !e.needBin() {
			os.Exit(1)
		}
		if e.portAlive() {
			e.prettyStatus()
			os.Exit(0)
		}
		e.start(args[1:])
		time.Sleep(2 * time.Second)
		e.prettyStatus()
	case "stop":
		e.stop()
		fmt.Println("nimroute stopped")
	case "restart":
		if !e.needBin() {
			os.Exit(1)
		}
		e.stop()
		time.Sleep(time.Second)
		e.start(args[1:])
		time.Sleep(2 * time.Second)
		e.prettyStatus()
	case "status":
		code := e.prettyStatus()
		if arg(1) == "tail" {
			nimStatusTail(e)
			os.Exit(0)
		}
		os.Exit(code)
	case "status-tail":
		nimStatusTail(e)
	case "logs":
		e.prettyStatus()
		if err := nimLogs(e, 10); err != nil {
			fmt.Printf("(log read failed: %v)\n", err)
			os.Exit(1)
		}
	case "log":
		switch a := arg(1); {
		case a == "" || a == "tail" || a == "f":
			if _, err := os.Stat(e.logFile); err != nil {
				fmt.Printf("no log yet (%s)\n", e.logFile)
				return
			}
			if err := followLog(e.logFile); err != nil {
				nimFatal("cannot follow "+e.logFile, err)
			}
		case a[0] >= '0' && a[0] <= '9':
			n, err := strconv.Atoi(a)
			lines, rerr := tailFile(e.logFile, n)
			if rerr != nil || err != nil {
				fmt.Printf("no log yet (%s)\n", e.logFile)
				return
			}
			fmt.Printf("\n  Access log (last %d):\n", n)
			for _, ln := range lines {
				fmt.Println("    " + ln)
			}
		default:
			fmt.Fprintf(os.Stderr, "nim: 'log' takes a line count or 'tail', got '%s'\n", a)
			fmt.Fprintln(os.Stderr, "     usage: ./nim log [N|tail]")
			os.Exit(1)
		}
	case "tail":
		if _, err := os.Stat(e.logFile); err != nil {
			fmt.Printf("no log yet (%s)\n", e.logFile)
			return
		}
		if err := followLog(e.logFile); err != nil {
			nimFatal("cannot follow "+e.logFile, err)
		}
	case "freepi":
		switch a := arg(1); a {
		case "auth":
			py := e.needPy("freepi auth")
			nimExec([]string{py, filepath.Join(e.dir, "scripts", "freepi_login.py"),
				"--into-config", filepath.Join(e.dir, "config.yml")})
		case "":
			fmt.Fprintln(os.Stderr, "nim: 'freepi' needs a subcommand")
			fmt.Fprintln(os.Stderr, "     usage: ./nim freepi auth")
			os.Exit(1)
		default:
			fmt.Fprintf(os.Stderr, "nim: unknown freepi subcommand '%s'\n", a)
			fmt.Fprintln(os.Stderr, "     usage: ./nim freepi auth")
			os.Exit(1)
		}
	case "models":
		switch a := arg(1); {
		case a == "":
			if err := nimModels(e, 0); err != nil {
				fmt.Printf("(status fetch failed: %v)\n", err)
				os.Exit(1)
			}
		case a[0] >= '0' && a[0] <= '9':
			n, err := strconv.Atoi(a)
			if err != nil {
				fmt.Println("usage: ./nim models [N]")
				os.Exit(1)
			}
			if err := nimModels(e, n); err != nil {
				fmt.Printf("(status fetch failed: %v)\n", err)
				os.Exit(1)
			}
		case a == "sync":
			if !e.needBin() {
				os.Exit(1)
			}
			argv := []string{e.bin, "models", "sync", "--config", filepath.Join(e.dir, "config.yml")}
			nimExec(append(argv, args[2:]...))
		default:
			fmt.Fprintf(os.Stderr, "nim: unknown models subcommand '%s'\n", a)
			fmt.Fprintln(os.Stderr, "     usage: ./nim models [N] | ./nim models sync")
			os.Exit(1)
		}
	case "install":
		if !e.needBin() {
			os.Exit(1)
		}
		if e.portAlive() {
			e.prettyStatus()
			os.Exit(0)
		}
		e.start(nil)
		time.Sleep(2 * time.Second)
		e.prettyStatus()
		fmt.Printf("installed (%s). see deploy/ for system-wide setup.\n", e.mode)
	case "uninstall":
		e.stop()
		_ = os.Remove(e.plist)
		_ = os.Remove(e.systemd)
		fmt.Println("nimroute uninstalled")
	case "help", "-h", "--help":
		fmt.Print(nimUsage)
	default:
		fmt.Fprintf(os.Stderr, "nim: unknown command '%s'\n\n", cmd)
		fmt.Fprint(os.Stderr, nimUsage)
		os.Exit(1)
	}
}
