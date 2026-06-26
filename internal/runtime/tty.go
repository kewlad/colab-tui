package runtime

import (
	"encoding/base64"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/netanelbi/colab-tui/internal/config"
)

// wrapCommand builds the tty input line for a synchronous Exec/ExecStream.
// The user command is base64-encoded and run inside its OWN bash subshell so
// that (a) no shell quoting/escaping of cmd is needed and (b) the user command
// aborting — via `set -e`, a non-zero exit, or `exit` — cannot prevent the
// trailing end-marker echo from running. Without this isolation, a command that
// exits the shell never emits the end marker and the reader blocks until the
// full timeout (the "hangs at the end even though the work finished" failure).
func wrapCommand(startMarker, cmd, endMarker string) string {
	enc := base64.StdEncoding.EncodeToString([]byte(cmd))
	return fmt.Sprintf("echo %s; bash -c \"$(echo %s | base64 -d)\"; echo %s\n", startMarker, enc, endMarker)
}

// TTY holds a WebSocket connection to the Colab TTY endpoint.
type TTY struct {
	conn    *websocket.Conn
	baseURL string
	token   string
}

type ttyMsg struct {
	Data string `json:"data"`
}

// NewTTY dials wss://{baseURL}/colab/tty, sends an initial resize, and drains
// any buffered scrollback so subsequent Exec calls start clean.
func NewTTY(baseURL, token string) (*TTY, error) {
	wsURL := toWS(baseURL) + "/colab/tty"
	hdr := http.Header{}
	hdr.Set(config.HProxyToken, token)
	// The terminal WS carries only the proxy token — upstream deliberately does
	// NOT send X-Colab-Client-Agent here (unlike the kernel WS).

	conn, err := dialWSRetry(wsURL, hdr)
	if err != nil {
		return nil, fmt.Errorf("tty dial: %w", err)
	}

	resize := map[string]int{"cols": 200, "rows": 50}
	if err := conn.WriteJSON(resize); err != nil {
		conn.Close()
		return nil, fmt.Errorf("tty resize: %w", err)
	}

	t := &TTY{conn: conn, baseURL: baseURL, token: token}
	t.sync()
	return t, nil
}

// sync sends a unique marker and reads until it appears, discarding all
// buffered scrollback. This leaves the connection clean for the next Exec.
func (t *TTY) sync() {
	marker := "SYNC_" + randHex(12)
	t.conn.WriteJSON(ttyMsg{Data: "echo " + marker + "\n"})
	t.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		var msg ttyMsg
		if err := t.conn.ReadJSON(&msg); err != nil {
			break
		}
		if strings.Contains(msg.Data, marker) {
			break
		}
	}
	t.conn.SetReadDeadline(time.Time{})
}

// Exec runs cmd synchronously and returns the cleaned output.
// Uses a start+end marker pair so we only capture output from THIS command,
// ignoring any leftover scrollback.
func (t *TTY) Exec(cmd string, timeout time.Duration) (string, error) {
	id := randHex(12)
	startMarker := "START_" + id
	endMarker := "END_" + id

	// Echo start marker, run command (isolated subshell), echo end marker — one payload.
	payload := ttyMsg{Data: wrapCommand(startMarker, cmd, endMarker)}
	if err := t.conn.WriteJSON(payload); err != nil {
		return "", fmt.Errorf("tty exec write: %w", err)
	}

	var buf strings.Builder
	deadline := time.Now().Add(timeout)
	t.conn.SetReadDeadline(deadline)

	// The end marker appears in the echoed command AND in the actual output.
	// We need to find at least 2 occurrences of endMarker (command echo + output),
	// or specifically: endMarker on its own line after startMarker output.
	for {
		var msg ttyMsg
		if err := t.conn.ReadJSON(&msg); err != nil {
			return "", fmt.Errorf("tty exec read: %w", err)
		}
		buf.WriteString(msg.Data)
		full := StripAnsi(buf.String())
		// Count occurrences of endMarker — need at least 2
		// (one in command echo, one in actual output)
		if strings.Count(full, endMarker) >= 2 {
			break
		}
	}

	return extractBetweenMarkers(buf.String(), startMarker, endMarker, cmd), nil
}

// extractBetweenMarkers pulls only the content between start and end markers,
// skipping the echoed command line.
func extractBetweenMarkers(raw, startMarker, endMarker, cmd string) string {
	clean := StripAnsi(raw)

	// The start marker appears twice: once in the echoed command line
	// ("echo START_xxx; cmd; echo END_xxx") and once as the actual output.
	// Find the SECOND occurrence — the actual output line.
	firstIdx := strings.Index(clean, startMarker)
	if firstIdx < 0 {
		return CleanOutput(clean, cmd, endMarker)
	}
	secondIdx := strings.Index(clean[firstIdx+len(startMarker):], startMarker)
	startIdx := firstIdx
	if secondIdx >= 0 {
		startIdx = firstIdx + len(startMarker) + secondIdx
	}

	// Skip past the start marker line
	after := clean[startIdx+len(startMarker):]
	if nl := strings.Index(after, "\n"); nl >= 0 {
		after = after[nl+1:]
	}

	// Find end marker
	endIdx := strings.Index(after, endMarker)
	if endIdx < 0 {
		return strings.TrimSpace(after)
	}
	content := after[:endIdx]

	// Trim trailing whitespace/prompts
	return strings.TrimRight(content, "\r\n ")
}

// ExecStream runs cmd and writes each received chunk to w, stopping at the marker.
// It waits for the SECOND occurrence of startMarker (the actual echo output, not the
// command echo) before streaming, and strips \r characters for clean output.
func (t *TTY) ExecStream(cmd string, w io.Writer, timeout time.Duration) error {
	id := randHex(12)
	startMarker := "START_" + id
	endMarker := "END_" + id

	payload := ttyMsg{Data: wrapCommand(startMarker, cmd, endMarker)}
	if err := t.conn.WriteJSON(payload); err != nil {
		return fmt.Errorf("tty stream write: %w", err)
	}

	deadline := time.Now().Add(timeout)
	t.conn.SetReadDeadline(deadline)

	startCount := 0
	started := false
	lastEndedNewline := false
	var accumulated strings.Builder

	for {
		var msg ttyMsg
		if err := t.conn.ReadJSON(&msg); err != nil {
			return fmt.Errorf("tty stream read: %w", err)
		}
		chunk := StripAnsi(msg.Data)
		chunk = strings.ReplaceAll(chunk, "\r", "")
		accumulated.WriteString(chunk)

		if !started {
			full := accumulated.String()
			// Count occurrences of startMarker — need the 2nd one
			// (1st is in command echo, 2nd is actual output)
			count := strings.Count(full, startMarker)
			if count > startCount {
				startCount = count
			}
			if startCount >= 2 {
				started = true
				// Find the second occurrence
				firstIdx := strings.Index(full, startMarker)
				secondIdx := strings.Index(full[firstIdx+len(startMarker):], startMarker)
				startIdx := firstIdx + len(startMarker) + secondIdx
				after := full[startIdx+len(startMarker):]
				// Skip the rest of the marker line
				if nl := strings.Index(after, "\n"); nl >= 0 {
					after = after[nl+1:]
				}
				// Check if end marker already present
				if endIdx := strings.Index(after, endMarker); endIdx >= 0 {
					content := cleanStreamOutput(after[:endIdx])
					if content != "" {
						fmt.Fprint(w, content)
					}
					return nil
				}
				content := cleanStreamOutput(after)
				if content != "" {
					fmt.Fprint(w, content)
				}
				accumulated.Reset()
			}
			continue
		}

		// Already started — check for end marker in accumulated
		accumulated2 := accumulated.String()
		if strings.Contains(accumulated2, endMarker) {
			before, _, _ := strings.Cut(accumulated2, endMarker)
			content := cleanStreamOutput(before)
			if lastEndedNewline {
				content = strings.TrimLeft(content, "\n")
			}
			if content != "" {
				fmt.Fprint(w, content)
			}
			return nil
		}
		// Flush what we have
		content := cleanStreamOutput(chunk)
		if lastEndedNewline {
			content = strings.TrimLeft(content, "\n")
		}
		if content != "" {
			fmt.Fprint(w, content)
			lastEndedNewline = strings.HasSuffix(content, "\n")
		}
		accumulated.Reset()
	}
}

// cleanStreamOutput collapses consecutive blank lines into single newlines.
func cleanStreamOutput(s string) string {
	for strings.Contains(s, "\n\n") {
		s = strings.ReplaceAll(s, "\n\n", "\n")
	}
	return s
}

// ExecBackground launches cmd via nohup in the background and returns the log path.
// The user command is base64-decoded into its own script and run under `bash` (same
// isolation as the synchronous wrapCommand path), so env-prefixes (VAR=x cmd), pipes,
// redirects, `&&`, and `;` all work. The previous `nohup %s` form passed the command
// straight to nohup, so `VAR=x python ...` made nohup try to exec the literal "VAR=x"
// ("nohup: failed to run command 'VAR=x'").
func (t *TTY) ExecBackground(cmd string) (string, error) {
	id := randHex(6)
	logPath := "/tmp/colab-tui/" + id + ".log"
	scriptPath := "/tmp/colab-tui/" + id + ".sh"
	enc := base64.StdEncoding.EncodeToString([]byte(cmd))
	wrapped := fmt.Sprintf("mkdir -p /tmp/colab-tui && echo %s | base64 -d > %s && nohup bash %s > %s 2>&1 &\n",
		enc, scriptPath, scriptPath, logPath)
	payload := ttyMsg{Data: wrapped}
	if err := t.conn.WriteJSON(payload); err != nil {
		return "", fmt.Errorf("tty bg write: %w", err)
	}
	return logPath, nil
}

// Close closes the underlying WebSocket connection.
func (t *TTY) Close() error {
	return t.conn.Close()
}

// dialWSRetry dials a websocket, retrying transient handshake/connection failures.
// Colab's proxy intermittently returns "bad handshake" / closes 1006 under load or
// right after a runtime (re)assignment; a few short backed-off retries turn those
// spurious one-shot failures into success instead of surfacing them to the user.
func dialWSRetry(wsURL string, hdr http.Header) (*websocket.Conn, error) {
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 750 * time.Millisecond)
		}
		conn, _, err := websocket.DefaultDialer.Dial(wsURL, hdr)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		if !isTransientDialErr(err) {
			break
		}
	}
	return nil, lastErr
}

// isTransientDialErr reports whether a websocket dial error is the kind that a
// retry can plausibly recover from (vs. e.g. a 404 for a deallocated runtime).
func isTransientDialErr(err error) bool {
	s := err.Error()
	for _, m := range []string{"bad handshake", "1006", "1005", "EOF", "i/o timeout", "connection reset", "connection refused"} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// toWS converts an http(s) URL to ws(s).
func toWS(u string) string {
	u = strings.TrimRight(u, "/")
	if strings.HasPrefix(u, "https://") {
		return "wss://" + strings.TrimPrefix(u, "https://")
	}
	if strings.HasPrefix(u, "http://") {
		return "ws://" + strings.TrimPrefix(u, "http://")
	}
	return "wss://" + u
}

func randHex(n int) string {
	const chars = "0123456789abcdef"
	b := make([]byte, n)
	for i := range b {
		b[i] = chars[rand.Intn(len(chars))]
	}
	return string(b)
}
