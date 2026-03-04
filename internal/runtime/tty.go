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

// NewTTY dials wss://{baseURL}/colab/tty and sends an initial resize message.
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

	return &TTY{conn: conn, baseURL: baseURL, token: token}, nil
}

// Exec runs cmd synchronously and returns the cleaned output.
func (t *TTY) Exec(cmd string, timeout time.Duration) (string, error) {
	marker := "DONE_" + randHex(8)
	payload := ttyMsg{Data: cmd + "; echo " + marker + "\n"}
	if err := t.conn.WriteJSON(payload); err != nil {
		return "", fmt.Errorf("tty exec write: %w", err)
	}

	var buf strings.Builder
	deadline := time.Now().Add(timeout)
	t.conn.SetReadDeadline(deadline)

	for {
		var msg ttyMsg
		if err := t.conn.ReadJSON(&msg); err != nil {
			return "", fmt.Errorf("tty exec read: %w", err)
		}
		buf.WriteString(msg.Data)
		if strings.Contains(buf.String(), marker) {
			break
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("tty exec timeout")
		}
	}

	return CleanOutput(buf.String(), cmd, marker), nil
}

// ExecStream runs cmd and writes each received chunk to w, stopping at the marker.
func (t *TTY) ExecStream(cmd string, w io.Writer, timeout time.Duration) error {
	marker := "DONE_" + randHex(8)
	payload := ttyMsg{Data: cmd + "; echo " + marker + "\n"}
	if err := t.conn.WriteJSON(payload); err != nil {
		return fmt.Errorf("tty stream write: %w", err)
	}

	deadline := time.Now().Add(timeout)
	t.conn.SetReadDeadline(deadline)

	for {
		var msg ttyMsg
		if err := t.conn.ReadJSON(&msg); err != nil {
			return fmt.Errorf("tty stream read: %w", err)
		}
		chunk := StripAnsi(msg.Data)
		if strings.Contains(chunk, marker) {
			// Write the part before the marker.
			before, _, _ := strings.Cut(chunk, marker)
			if before != "" {
				fmt.Fprint(w, before)
			}
			return nil
		}
		fmt.Fprint(w, chunk)
		if time.Now().After(deadline) {
			return fmt.Errorf("tty stream timeout")
		}
	}
}

// ExecBackground launches cmd via nohup in the background and returns the log path.
func (t *TTY) ExecBackground(cmd string) (string, error) {
	// Derive a short ID from the command.
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
