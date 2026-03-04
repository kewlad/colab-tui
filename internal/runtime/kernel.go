package runtime

import (
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
	conn     *websocket.Conn
	kernelID string
	baseURL  string
	token    string
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
		return nil, err
	}

	wsURL := toWS(baseURL) + "/api/kernels/" + kernelID + "/channels"
	hdr := http.Header{}
	hdr.Set(config.HProxyToken, token)
	hdr.Set(config.HAgent, config.HAgentVal)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, hdr)
	if err != nil {
		return nil, fmt.Errorf("kernel dial: %w", err)
	}

	return &Kernel{conn: conn, kernelID: kernelID, baseURL: baseURL, token: token}, nil
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
	sessionID := uuid.NewString()

	msg := jupyterMsg{
		Header: map[string]string{
			"msg_id":   msgID,
			"msg_type": "execute_request",
			"session":  sessionID,
		},
		ParentHeader: map[string]interface{}{},
		Metadata:     map[string]interface{}{},
		Content: map[string]interface{}{
			"code":          code,
			"silent":        false,
			"store_history": true,
		},
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

// Close closes the underlying WebSocket connection.
func (k *Kernel) Close() error {
	return k.conn.Close()
}
