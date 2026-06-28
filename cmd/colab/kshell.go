package main

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
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

// kernelShell runs a shell command on the runtime through the Jupyter KERNEL
// (not the TTY). The kernel uses framed Jupyter messages, so it is immune to the
// PTY-marker scraping and terminal wedges that plague the TTY path. The command
// is base64'd to avoid all quoting, run via subprocess, and both stdout+stderr
// are returned.
func kernelShell(baseURL, token, label, cmd string, timeout time.Duration) (string, error) {
	k, err := runtime.NewKernel(baseURL, token, label)
	if err != nil {
		return "", fmt.Errorf("kernel connect: %w", err)
	}
	defer k.Close()
	enc := base64.StdEncoding.EncodeToString([]byte(cmd))
	code := fmt.Sprintf(`import subprocess as _sp, base64 as _b64, sys as _s
_p = _sp.run(_b64.b64decode(%q).decode(), shell=True, capture_output=True, text=True)
_s.stdout.write(_p.stdout)
_s.stdout.write(_p.stderr)`, enc)
	return k.Execute(code, timeout)
}

// runtimeShell runs a shell command on the runtime, preferring the TTY but
// automatically falling back to the kernel when the TTY is wedged. Output is
// written to w. This is the resilient primitive the exec/cp/rclone commands use
// so a stuck terminal no longer blocks the whole workflow.
func runtimeShell(baseURL, token, label, cmd string, timeout time.Duration, w io.Writer) error {
	tty, terr := runtime.NewTTY(baseURL, token)
	if terr == nil {
		defer tty.Close()
		err := tty.ExecStream(cmd, w, timeout)
		if err == nil {
			return nil
		}
		if !isTTYWedge(err) {
			return err
		}
		fmt.Fprintln(os.Stderr, "[colab] TTY unresponsive — falling back to the kernel channel...")
	} else if !isTTYWedge(terr) {
		return fmt.Errorf("connect TTY: %w", terr)
	}
	out, err := kernelShell(baseURL, token, label, cmd, timeout)
	if err != nil {
		return err
	}
	fmt.Fprint(w, out)
	return nil
}
