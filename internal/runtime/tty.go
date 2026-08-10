package runtime

import (
	"encoding/base64"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/netanelbi/colab-tui/internal/config"
)

// cmdTimeoutErr builds the error for a command that produced output but ran past
// the caller's timeout. This is NOT a TTY wedge (the terminal was demonstrably
// working), so the message deliberately avoids the isTTYWedge markers — the
// exec fallback must not re-run a command that already partially executed.
func cmdTimeoutErr(timeout time.Duration) error {
	return fmt.Errorf("command still running after --timeout %s — it is likely killed as this TTY session closes; "+
		"raise --timeout, or use `colab exec --bg` / `colab python --detach` for long jobs", timeout)
}

// isReadTimeout reports whether a websocket read error is a deadline expiry (as
// opposed to a closed/failed connection).
func isReadTimeout(err error) bool {
	ne, ok := err.(net.Error)
	return ok && ne.Timeout()
}

// StatusUnknown is returned as the exit status when the remote command's `$?`
// could not be read back off the TTY (a truncated end marker). It is distinct
// from 0 so callers never report a failure as success, nor vice versa.
const StatusUnknown = -1

// wrapCommand builds the tty input line for a synchronous Exec/ExecStream.
// The user command is base64-encoded and run inside its OWN bash subshell so
// that (a) no shell quoting/escaping of cmd is needed and (b) the user command
// aborting — via `set -e`, a non-zero exit, or `exit` — cannot prevent the
// trailing end-marker echo from running. Without this isolation, a command that
// exits the shell never emits the end marker and the reader blocks until the
// full timeout (the "hangs at the end even though the work finished" failure).
//
// The end marker carries the subshell's `$?` (`END_xxx:7`) so the caller can
// propagate the REMOTE exit status. Without it every `colab exec` exits 0 and
// cannot be used as a shell condition — `until colab exec "grep -q DONE log"`
// would exit on its first iteration looking like the condition was met.
func wrapCommand(startMarker, cmd, endMarker string) string {
	enc := base64.StdEncoding.EncodeToString([]byte(cmd))
	return fmt.Sprintf("echo %s; bash -c \"$(echo %s | base64 -d)\"; echo %s:$?\n", startMarker, enc, endMarker)
}

// parseExitStatus extracts the status appended to the end marker
// (`END_xxx:7`). The command echo also contains the marker but is followed by
// the literal `:$?`, so only a marker followed by digits matches; the LAST such
// occurrence wins. Returns StatusUnknown when no digits followed the marker.
func parseExitStatus(clean, endMarker string) int {
	status := StatusUnknown
	for off := 0; ; {
		i := strings.Index(clean[off:], endMarker+":")
		if i < 0 {
			return status
		}
		off += i + len(endMarker) + 1
		j := off
		for j < len(clean) && clean[j] >= '0' && clean[j] <= '9' {
			j++
		}
		if j > off {
			if n, err := strconv.Atoi(clean[off:j]); err == nil {
				status = n
			}
		}
	}
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

// statusGrace is how long to keep reading after the end marker appears, waiting
// for the `:$?` digits that may land in a later websocket frame.
const statusGrace = 2 * time.Second

// Exec runs cmd synchronously and returns the cleaned output and the remote
// command's exit status (StatusUnknown if the status digits never arrived).
// Uses a start+end marker pair so we only capture output from THIS command,
// ignoring any leftover scrollback.
func (t *TTY) Exec(cmd string, timeout time.Duration) (string, int, error) {
	id := randHex(12)
	startMarker := "START_" + id
	endMarker := "END_" + id

	// Echo start marker, run command (isolated subshell), echo end marker — one payload.
	payload := ttyMsg{Data: wrapCommand(startMarker, cmd, endMarker)}
	if err := t.conn.WriteJSON(payload); err != nil {
		return "", StatusUnknown, fmt.Errorf("tty exec write: %w", err)
	}

	var buf strings.Builder
	deadline := time.Now().Add(timeout)
	t.conn.SetReadDeadline(deadline)

	// The end marker appears in the echoed command AND in the actual output.
	// We need to find at least 2 occurrences of endMarker (command echo + output),
	// or specifically: endMarker on its own line after startMarker output.
	status := StatusUnknown
	inGrace := false
	for {
		var msg ttyMsg
		if err := t.conn.ReadJSON(&msg); err != nil {
			// The end marker already landed; only its status digits are missing.
			// Report the output rather than failing the whole command.
			if inGrace && isReadTimeout(err) {
				break
			}
			// If the TTY already echoed our command back, the terminal is alive
			// and a deadline expiry means the COMMAND outlived the timeout.
			if isReadTimeout(err) && strings.Count(StripAnsi(buf.String()), startMarker) >= 1 {
				return "", StatusUnknown, cmdTimeoutErr(timeout)
			}
			return "", StatusUnknown, fmt.Errorf("tty exec read: %w", err)
		}
		buf.WriteString(msg.Data)
		full := StripAnsi(buf.String())
		// Count occurrences of endMarker — need at least 2
		// (one in command echo, one in actual output)
		if strings.Count(full, endMarker) >= 2 {
			if status = parseExitStatus(full, endMarker); status != StatusUnknown {
				break
			}
			if !inGrace {
				inGrace = true
				t.conn.SetReadDeadline(time.Now().Add(statusGrace))
			}
		}
	}

	return extractBetweenMarkers(buf.String(), startMarker, endMarker, cmd), status, nil
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

// readStatus keeps reading after the end marker has appeared, until the `:$?`
// digits arrive or statusGrace expires. seen must be the ANSI-stripped text
// containing the marker.
func (t *TTY) readStatus(seen, endMarker string) int {
	if s := parseExitStatus(seen, endMarker); s != StatusUnknown {
		return s
	}
	t.conn.SetReadDeadline(time.Now().Add(statusGrace))
	tail := seen
	for {
		var msg ttyMsg
		if err := t.conn.ReadJSON(&msg); err != nil {
			return StatusUnknown
		}
		tail += StripAnsi(msg.Data)
		if s := parseExitStatus(tail, endMarker); s != StatusUnknown {
			return s
		}
	}
}

// ExecStream runs cmd and writes each received chunk to w, stopping at the marker.
// It waits for the SECOND occurrence of startMarker (the actual echo output, not the
// command echo) before streaming, and strips \r characters for clean output.
// Returns the remote command's exit status (StatusUnknown if unreadable).
func (t *TTY) ExecStream(cmd string, w io.Writer, timeout time.Duration) (int, error) {
	id := randHex(12)
	startMarker := "START_" + id
	endMarker := "END_" + id

	payload := ttyMsg{Data: wrapCommand(startMarker, cmd, endMarker)}
	if err := t.conn.WriteJSON(payload); err != nil {
		return StatusUnknown, fmt.Errorf("tty stream write: %w", err)
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
			// Same distinction as Exec: an echoed command proves the TTY works,
			// so a deadline expiry is the command outliving the timeout — not a
			// wedge to fall back (and re-run the command!) on.
			if isReadTimeout(err) && (started || startCount >= 1) {
				return StatusUnknown, cmdTimeoutErr(timeout)
			}
			return StatusUnknown, fmt.Errorf("tty stream read: %w", err)
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
					return t.readStatus(after[endIdx:], endMarker), nil
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
		if idx := strings.Index(accumulated2, endMarker); idx >= 0 {
			content := cleanStreamOutput(accumulated2[:idx])
			if lastEndedNewline {
				content = strings.TrimLeft(content, "\n")
			}
			if content != "" {
				fmt.Fprint(w, content)
			}
			return t.readStatus(accumulated2[idx:], endMarker), nil
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
