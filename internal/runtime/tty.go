package runtime

import (
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/netanelbi/colab-tui/internal/config"
)

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
	hdr.Set(config.HAgent, config.HAgentVal)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, hdr)
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

	// Echo start marker, run command, echo end marker — all as one payload.
	payload := ttyMsg{Data: fmt.Sprintf("echo %s; %s; echo %s\n", startMarker, cmd, endMarker)}
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
func (t *TTY) ExecStream(cmd string, w io.Writer, timeout time.Duration) error {
	id := randHex(12)
	startMarker := "START_" + id
	endMarker := "END_" + id

	payload := ttyMsg{Data: fmt.Sprintf("echo %s; %s; echo %s\n", startMarker, cmd, endMarker)}
	if err := t.conn.WriteJSON(payload); err != nil {
		return fmt.Errorf("tty stream write: %w", err)
	}

	deadline := time.Now().Add(timeout)
	t.conn.SetReadDeadline(deadline)

	started := false
	var accumulated strings.Builder

	for {
		var msg ttyMsg
		if err := t.conn.ReadJSON(&msg); err != nil {
			return fmt.Errorf("tty stream read: %w", err)
		}
		chunk := StripAnsi(msg.Data)
		accumulated.WriteString(chunk)
		full := accumulated.String()

		if !started {
			// Wait for start marker
			if idx := strings.Index(full, startMarker); idx >= 0 {
				started = true
				after := full[idx+len(startMarker):]
				if nl := strings.Index(after, "\n"); nl >= 0 {
					after = after[nl+1:]
				}
				// Check if end marker is already in this chunk
				if endIdx := strings.Index(after, endMarker); endIdx >= 0 {
					fmt.Fprint(w, after[:endIdx])
					return nil
				}
				fmt.Fprint(w, after)
				accumulated.Reset()
			}
			continue
		}

		// Already started — check for end marker
		if strings.Contains(chunk, endMarker) {
			before, _, _ := strings.Cut(chunk, endMarker)
			if before != "" {
				fmt.Fprint(w, before)
			}
			return nil
		}
		fmt.Fprint(w, chunk)
		accumulated.Reset()
	}
}

// ExecBackground launches cmd via nohup in the background and returns the log path.
func (t *TTY) ExecBackground(cmd string) (string, error) {
	id := randHex(6)
	logPath := "/tmp/colab-tui/" + id + ".log"
	wrapped := fmt.Sprintf("mkdir -p /tmp/colab-tui && nohup %s > %s 2>&1 &\n", cmd, logPath)
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
