package main

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/netanelbi/colab-tui/internal/runtime"
)

// isTTYWedge reports whether an error from the TTY exec path indicates the
// runtime's terminal websocket is unresponsive (accepts the connection but
// never sends output back) rather than a genuine command failure. Observed as
// read i/o timeouts and abnormal websocket closes while the Jupyter kernel
// channel stays perfectly responsive.
func isTTYWedge(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	for _, m := range []string{"i/o timeout", "1005", "tty exec read", "tty stream read", "websocket: close", "tty dial"} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// rcMarker terminates kernelShell output with the remote command's returncode.
// The kernel channel has no `$?` to scrape, so the cell prints it explicitly.
const rcMarker = "__COLAB_RC__"

// splitRC strips the trailing rc marker line from kernel-shell output and
// returns the remaining output plus the exit status (StatusUnknown if the
// marker is missing — e.g. the cell itself raised before reaching it).
func splitRC(out string) (string, int) {
	i := strings.LastIndex(out, rcMarker)
	if i < 0 {
		return out, runtime.StatusUnknown
	}
	rc, err := strconv.Atoi(strings.TrimSpace(out[i+len(rcMarker):]))
	if err != nil {
		return out, runtime.StatusUnknown
	}
	return strings.TrimSuffix(out[:i], "\n"), rc
}

// kernelShell runs a shell command on the runtime through the Jupyter KERNEL
// (not the TTY). The kernel uses framed Jupyter messages, so it is immune to the
// PTY-marker scraping and terminal wedges that plague the TTY path. The command
// is base64'd to avoid all quoting, run via subprocess, and both stdout+stderr
// are returned along with the remote exit status.
func kernelShell(baseURL, token, label, cmd string, timeout time.Duration) (string, int, error) {
	k, err := runtime.NewKernel(baseURL, token, label)
	if err != nil {
		return "", runtime.StatusUnknown, fmt.Errorf("kernel connect: %w", err)
	}
	defer k.Close()
	enc := base64.StdEncoding.EncodeToString([]byte(cmd))
	// Every write is bound to `_`: sys.stdout.write returns a character count,
	// and IPython echoes a cell's last expression — so an unbound trailing write
	// appended its own length to the command's output (14 after the rc marker,
	// which then parsed as the exit status).
	code := fmt.Sprintf(`import subprocess as _sp, base64 as _b64, sys as _s
_p = _sp.run(_b64.b64decode(%q).decode(), shell=True, capture_output=True, text=True)
_ = _s.stdout.write(_p.stdout)
_ = _s.stdout.write(_p.stderr)
_ = _s.stdout.write("\n%s%%d" %% _p.returncode)`, enc, rcMarker)
	out, err := k.Execute(code, timeout)
	if err != nil {
		return "", runtime.StatusUnknown, err
	}
	out, rc := splitRC(out)
	return out, rc, nil
}

// runtimeShell runs a shell command on the runtime, preferring the TTY but
// automatically falling back to the kernel when the TTY is wedged. Output is
// written to w. This is the resilient primitive the exec/cp/rclone commands use
// so a stuck terminal no longer blocks the whole workflow. It returns the
// REMOTE command's exit status so callers can propagate it (StatusUnknown when
// the status could not be read).
func runtimeShell(baseURL, token, label, cmd string, timeout time.Duration, w io.Writer) (int, error) {
	tty, terr := runtime.NewTTY(baseURL, token)
	if terr == nil {
		defer tty.Close()
		rc, err := tty.ExecStream(cmd, w, timeout)
		if err == nil {
			return rc, nil
		}
		if !isTTYWedge(err) {
			return runtime.StatusUnknown, err
		}
		fmt.Fprintln(os.Stderr, "[colab] TTY unresponsive — falling back to the kernel channel...")
	} else if !isTTYWedge(terr) {
		return runtime.StatusUnknown, fmt.Errorf("connect TTY: %w", terr)
	}
	out, rc, err := kernelShell(baseURL, token, label, cmd, timeout)
	if err != nil {
		return runtime.StatusUnknown, err
	}
	fmt.Fprint(w, out)
	return rc, nil
}

// reportStatus writes a one-line note when a remote command failed or its
// status was unreadable, and returns the error that makes `colab` exit with the
// remote status. A successful command returns nil.
func reportStatus(rc int, what string) error {
	switch {
	case rc == 0:
		return nil
	case rc == runtime.StatusUnknown:
		fmt.Fprintf(os.Stderr, "[colab] warning: %s exit status unavailable — treating as success\n", what)
		return nil
	default:
		fmt.Fprintf(os.Stderr, "[colab] %s exited %d\n", what, rc)
		return exitCode(rc)
	}
}
