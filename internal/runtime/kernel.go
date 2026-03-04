package runtime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/netanelbi/colab-tui/internal/config"
)

// Kernel holds a WebSocket connection to a Jupyter kernel.
type Kernel struct {
	conn      *websocket.Conn
	kernelID  string
	sessionID string
	baseURL   string
	token     string
}

// kernelInfo is used to parse the /api/kernels list response.
type kernelInfo struct {
	ID             string `json:"id"`
	ExecutionState string `json:"execution_state"`
}

// NewKernel fetches the first available kernel and dials its channel WebSocket.
func NewKernel(baseURL, token string) (*Kernel, error) {
	kernelID, err := fetchFirstKernelID(baseURL, token)
	if err != nil {
		// No kernels — try creating a session to start one
		kernelID, err = CreateSession(baseURL, token, "colab-tui")
		if err != nil {
			return nil, fmt.Errorf("no kernels and session creation failed: %w", err)
		}
	}

	sessionID := uuid.NewString()
	wsURL := toWS(baseURL) + "/api/kernels/" + kernelID + "/channels?session_id=" + sessionID
	hdr := http.Header{}
	hdr.Set(config.HProxyToken, token)
	hdr.Set(config.HAgent, config.HAgentVal)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, hdr)
	if err != nil {
		return nil, fmt.Errorf("kernel dial: %w", err)
	}

	k := &Kernel{conn: conn, kernelID: kernelID, sessionID: sessionID, baseURL: baseURL, token: token}

	// Send kernel_info_request and drain initial messages (required handshake).
	infoMsg := jupyterMsg{
		Header: map[string]string{
			"msg_id":   uuid.NewString(),
			"msg_type": "kernel_info_request",
			"session":  sessionID,
			"username": "",
			"version":  "5.2",
		},
		ParentHeader: map[string]interface{}{},
		Metadata:     map[string]interface{}{},
		Content:      map[string]interface{}{},
		Channel:      "shell",
	}
	if err := conn.WriteJSON(infoMsg); err != nil {
		conn.Close()
		return nil, fmt.Errorf("kernel info request: %w", err)
	}

	// Drain until kernel_info_reply
	conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	for {
		var resp map[string]interface{}
		if err := conn.ReadJSON(&resp); err != nil {
			conn.Close()
			return nil, fmt.Errorf("kernel handshake: %w", err)
		}
		if hdr, ok := resp["header"].(map[string]interface{}); ok {
			if mt, _ := hdr["msg_type"].(string); mt == "kernel_info_reply" {
				break
			}
		}
	}
	conn.SetReadDeadline(time.Time{}) // clear deadline

	return k, nil
}

func fetchFirstKernelID(baseURL, token string) (string, error) {
	url := strings.TrimRight(baseURL, "/") + "/api/kernels"
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", fmt.Errorf("kernel list request: %w", err)
	}
	req.Header.Set(config.HProxyToken, token)
	req.Header.Set(config.HAgent, config.HAgentVal)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("kernel list: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("kernel list read: %w", err)
	}

	var kernels []kernelInfo
	if err := json.Unmarshal(body, &kernels); err != nil {
		return "", fmt.Errorf("kernel list parse: %w", err)
	}
	if len(kernels) == 0 {
		return "", fmt.Errorf("no kernels available")
	}
	return kernels[0].ID, nil
}

// jupyterMsg is a simplified Jupyter wire protocol message.
type jupyterMsg struct {
	Header       map[string]string      `json:"header"`
	ParentHeader map[string]interface{} `json:"parent_header"`
	Metadata     map[string]interface{} `json:"metadata"`
	Content      map[string]interface{} `json:"content"`
	MsgType      string                 `json:"msg_type,omitempty"`
	Channel      string                 `json:"channel,omitempty"`
}

// Execute sends code to the kernel and collects output until execute_reply.
func (k *Kernel) Execute(code string, timeout time.Duration) (string, error) {
	msgID := uuid.NewString()

	msg := jupyterMsg{
		Header: map[string]string{
			"msg_id":   msgID,
			"msg_type": "execute_request",
			"session":  k.sessionID,
			"username": "",
			"version":  "5.2",
		},
		ParentHeader: map[string]interface{}{},
		Metadata:     map[string]interface{}{},
		Content: map[string]interface{}{
			"code":            code,
			"silent":          false,
			"store_history":   true,
			"allow_stdin":     false,
			"stop_on_error":   true,
		},
		Channel: "shell",
	}

	if err := k.conn.WriteJSON(msg); err != nil {
		return "", fmt.Errorf("kernel execute write: %w", err)
	}

	var outputParts []string
	deadline := time.Now().Add(timeout)
	k.conn.SetReadDeadline(deadline)

	for {
		var resp map[string]interface{}
		if err := k.conn.ReadJSON(&resp); err != nil {
			return "", fmt.Errorf("kernel execute read: %w", err)
		}

		hdr, _ := resp["header"].(map[string]interface{})
		msgType, _ := hdr["msg_type"].(string)
		content, _ := resp["content"].(map[string]interface{})

		switch msgType {
		case "stream":
			if text, ok := content["text"].(string); ok {
				outputParts = append(outputParts, text)
			}
		case "execute_result":
			if data, ok := content["data"].(map[string]interface{}); ok {
				if text, ok := data["text/plain"].(string); ok {
					outputParts = append(outputParts, text)
				}
			}
		case "error":
			ename, _ := content["ename"].(string)
			evalue, _ := content["evalue"].(string)
			return "", fmt.Errorf("kernel error %s: %s", ename, evalue)
		case "execute_reply":
			// Done collecting output.
			return strings.Join(outputParts, ""), nil
		}

		if time.Now().After(deadline) {
			return "", fmt.Errorf("kernel execute timeout")
		}
	}
}

// GetStatus returns the execution_state of the kernel.
func (k *Kernel) GetStatus() (string, error) {
	url := strings.TrimRight(k.baseURL, "/") + "/api/kernels/" + k.kernelID
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", fmt.Errorf("kernel status request: %w", err)
	}
	req.Header.Set(config.HProxyToken, k.token)
	req.Header.Set(config.HAgent, config.HAgentVal)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("kernel status: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("kernel status read: %w", err)
	}

	var info kernelInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return "", fmt.Errorf("kernel status parse: %w", err)
	}
	return info.ExecutionState, nil
}

// Restart restarts the kernel via the Jupyter REST API, freeing GPU memory.
// It closes the existing WebSocket and reconnects after the restart completes.
func (k *Kernel) Restart() error {
	k.conn.Close()

	url := strings.TrimRight(k.baseURL, "/") + "/api/kernels/" + k.kernelID + "/restart"
	req, err := http.NewRequest("POST", url, nil)
	if err != nil {
		return fmt.Errorf("kernel restart request: %w", err)
	}
	req.Header.Set(config.HProxyToken, k.token)
	req.Header.Set(config.HAgent, config.HAgentVal)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("kernel restart: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("kernel restart failed (%d): %s", resp.StatusCode, string(body))
	}

	// Wait for kernel to come back (poll status up to 30s)
	for i := 0; i < 30; i++ {
		time.Sleep(time.Second)
		state, err := k.getStatusHTTP()
		if err == nil && (state == "idle" || state == "starting") {
			break
		}
	}

	// Reconnect WebSocket (must include session_id for Colab proxy)
	wsURL := toWS(k.baseURL) + "/api/kernels/" + k.kernelID + "/channels?session_id=" + k.sessionID
	hdr := http.Header{}
	hdr.Set(config.HProxyToken, k.token)
	hdr.Set(config.HAgent, config.HAgentVal)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, hdr)
	if err != nil {
		return fmt.Errorf("kernel reconnect after restart: %w", err)
	}
	k.conn = conn
	return nil
}

// getStatusHTTP fetches kernel status without using the WebSocket.
func (k *Kernel) getStatusHTTP() (string, error) {
	url := strings.TrimRight(k.baseURL, "/") + "/api/kernels/" + k.kernelID
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set(config.HProxyToken, k.token)
	req.Header.Set(config.HAgent, config.HAgentVal)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var info kernelInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return "", err
	}
	return info.ExecutionState, nil
}

// Close closes the underlying WebSocket connection.
func (k *Kernel) Close() error {
	return k.conn.Close()
}

// CreateSession creates a Jupyter session on the runtime, which starts a kernel.
// The session path uses the "-jvsc-" marker so Colab's "Active Sessions" web UI
// recognizes it as a VS Code-style session and displays the notebook title.
// Colab's frontend regex: ^(?!.*fileId=)(.*)(-jvsc-|-intellij-).*(\.[^.]+)$
func CreateSession(baseURL, token, label string) (kernelID string, err error) {
	// Strip .ipynb suffix if already present to avoid double extension
	label = strings.TrimSuffix(label, ".ipynb")
	nbName := label + ".ipynb"

	// The path must contain "-jvsc-" for Colab's web UI to extract the title.
	// Format: {title}-jvsc-{id}.{ext} → Colab shows "{title}.{ext}"
	sessionPath := label + "-jvsc-" + uuid.NewString() + ".ipynb"

	sessionURL := strings.TrimRight(baseURL, "/") + "/api/sessions"

	body := map[string]interface{}{
		"name": nbName,
		"path": sessionPath,
		"type": "notebook",
		"kernel": map[string]string{
			"name": "python3",
		},
	}

	data, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("marshal session body: %w", err)
	}

	req, err := http.NewRequest("POST", sessionURL, bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("create session request: %w", err)
	}
	req.Header.Set(config.HProxyToken, token)
	req.Header.Set(config.HAgent, config.HAgentVal)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read session response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("create session failed (%d): %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Kernel struct {
			ID string `json:"id"`
		} `json:"kernel"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("parse session response: %w", err)
	}

	return result.Kernel.ID, nil
}
