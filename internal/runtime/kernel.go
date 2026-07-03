package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/netanelbi/colab-tui/internal/config"
)

// ErrNoKernels signals the runtime was reachable but reported ZERO kernels — the
// only condition under which creating a new session is correct (the runtime is
// fresh or its kernel died). A transient list failure (network/proxy/token blip)
// must NOT be mistaken for this, or every reconnect hiccup would mint a duplicate
// session on a machine whose kernel is still alive.
var ErrNoKernels = errors.New("runtime has no kernels")

// httpClient is the shared client for the Jupyter REST API. A timeout is set so
// a hung runtime can't block a command forever (the WebSocket paths manage their
// own deadlines separately).
var httpClient = &http.Client{Timeout: 30 * time.Second}

// Kernel holds a WebSocket connection to a Jupyter kernel.
type Kernel struct {
	conn      *websocket.Conn
	kernelID  string
	sessionID string
	baseURL   string
	token     string
}

// kernelInfo is used to parse the /api/kernels list and detail responses.
type kernelInfo struct {
	ID             string `json:"id"`
	ExecutionState string `json:"execution_state"`
}

// kernelGet performs an authenticated GET against the runtime's Jupyter REST API
// and unmarshals the JSON response into out. path is appended to baseURL.
// Failures are retried with a short backoff: the Colab proxy flakes (5xx, empty
// bodies) exactly when a runtime is being recycled, and a one-shot failure here
// used to surface as a bare "unexpected end of JSON input" that broke exec's
// kernel fallback mid-command.
func kernelGet(baseURL, token, path string, out interface{}) error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 750 * time.Millisecond)
		}
		lastErr = kernelGetOnce(baseURL, token, path, out)
		if lastErr == nil {
			return nil
		}
	}
	return lastErr
}

func kernelGetOnce(baseURL, token, path string, out interface{}) error {
	url := strings.TrimRight(baseURL, "/") + path
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set(config.HProxyToken, token)
	req.Header.Set(config.HAgent, config.HAgentVal)

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		snippet := strings.TrimSpace(string(body))
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, snippet)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("parse response (HTTP %d, %d bytes): %w", resp.StatusCode, len(body), err)
	}
	return nil
}

// dialChannel opens the kernel's channel WebSocket (session_id is required by the
// Colab proxy) with the standard auth headers and transient-failure retries.
func dialChannel(baseURL, token, kernelID, sessionID string) (*websocket.Conn, error) {
	wsURL := toWS(baseURL) + "/api/kernels/" + kernelID + "/channels?session_id=" + sessionID
	hdr := http.Header{}
	hdr.Set(config.HProxyToken, token)
	hdr.Set(config.HAgent, config.HAgentVal)
	return dialWSRetry(wsURL, hdr)
}

// NewKernel fetches the first available kernel (creating a session if none exist)
// and dials its channel WebSocket, completing the kernel_info handshake. label
// names any session it has to create, so an auto-recreated session is attributed
// to its runtime (e.g. "v16a") rather than a generic bucket. An empty label
// falls back to "colab-tui".
func NewKernel(baseURL, token, label string) (*Kernel, error) {
	if label == "" {
		label = "colab-tui"
	}
	kernelID, err := fetchFirstKernelID(baseURL, token)
	if err != nil {
		// Only create a session when the runtime CONFIRMS it has no kernels.
		// A transient list error (the common case right after a WS drop, when
		// keepalive reconnects) must reuse the still-alive kernel on the next
		// try — never spawn a duplicate session. That accumulation is exactly
		// the "spam" seen in Colab's Active Sessions panel.
		if !errors.Is(err, ErrNoKernels) {
			return nil, err // already wrapped "kernel list: ..." by FirstKernelState
		}
		kernelID, err = CreateSession(baseURL, token, label)
		if err != nil {
			return nil, fmt.Errorf("no kernels and session creation failed: %w", err)
		}
	}

	sessionID := uuid.NewString()
	conn, err := dialChannel(baseURL, token, kernelID, sessionID)
	if err != nil {
		return nil, fmt.Errorf("kernel dial: %w", err)
	}

	k := &Kernel{conn: conn, kernelID: kernelID, sessionID: sessionID, baseURL: baseURL, token: token}

	// Warm up the connection with a kernel_info_request, but DON'T require the
	// reply: kernel_info is serviced on the shell channel, so a BUSY kernel won't
	// answer until its running cell finishes. Waiting for it would make every
	// connect (status/health/exec) hang until the kernel goes idle. A short
	// deadline + proceed-on-timeout keeps connect fast; the connection is fully
	// usable regardless (a later execute_request just queues behind the busy cell).
	infoID := uuid.NewString()
	if err := conn.WriteJSON(newJupyterMsg(infoID, sessionID, "kernel_info_request", nil)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("kernel info request: %w", err)
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		var resp map[string]interface{}
		if err := conn.ReadJSON(&resp); err != nil {
			break // busy kernel (or slow) — proceed without the reply
		}
		if msgTypeOf(resp) == "kernel_info_reply" {
			break
		}
	}
	conn.SetReadDeadline(time.Time{}) // clear deadline

	return k, nil
}

// NewKernelAttach connects to the kernel WebSocket without the kernel_info
// handshake. Use this to attach to a busy kernel's IOPub stream.
func NewKernelAttach(baseURL, token string) (*Kernel, error) {
	kernelID, err := fetchFirstKernelID(baseURL, token)
	if err != nil {
		return nil, fmt.Errorf("no kernels: %w", err)
	}
	sessionID := uuid.NewString()
	conn, err := dialChannel(baseURL, token, kernelID, sessionID)
	if err != nil {
		return nil, fmt.Errorf("kernel dial: %w", err)
	}
	return &Kernel{conn: conn, kernelID: kernelID, sessionID: sessionID, baseURL: baseURL, token: token}, nil
}

func fetchFirstKernelID(baseURL, token string) (string, error) {
	id, _, err := FirstKernelState(baseURL, token)
	return id, err
}

// FirstKernelState returns the id and execution_state (busy/idle/starting) of the
// runtime's first kernel via the REST API, without opening a WebSocket or
// creating a session. An error means there are no kernels — a strong signal the
// runtime was reaped or never started one.
func FirstKernelState(baseURL, token string) (id, state string, err error) {
	var kernels []kernelInfo
	if err := kernelGet(baseURL, token, "/api/kernels", &kernels); err != nil {
		return "", "", fmt.Errorf("kernel list: %w", err)
	}
	if len(kernels) == 0 {
		return "", "", ErrNoKernels
	}
	return kernels[0].ID, kernels[0].ExecutionState, nil
}

// jupyterMsg is a simplified Jupyter wire protocol message.
type jupyterMsg struct {
	Header       map[string]string      `json:"header"`
	ParentHeader map[string]interface{} `json:"parent_header"`
	Metadata     map[string]interface{} `json:"metadata"`
	Content      map[string]interface{} `json:"content"`
	Channel      string                 `json:"channel,omitempty"`
}

// newJupyterMsg builds a shell-channel message of the given type. If content is
// nil an empty object is sent.
func newJupyterMsg(msgID, sessionID, msgType string, content map[string]interface{}) jupyterMsg {
	if content == nil {
		content = map[string]interface{}{}
	}
	return jupyterMsg{
		Header: map[string]string{
			"msg_id":   msgID,
			"msg_type": msgType,
			"session":  sessionID,
			"username": "",
			"version":  "5.2",
		},
		ParentHeader: map[string]interface{}{},
		Metadata:     map[string]interface{}{},
		Content:      content,
		Channel:      "shell",
	}
}

// executeContent is the standard content block for an execute_request.
func executeContent(code string) map[string]interface{} {
	return map[string]interface{}{
		"code":          code,
		"silent":        false,
		"store_history": true,
		"allow_stdin":   false,
		"stop_on_error": true,
	}
}

// msgTypeOf / parentIDOf / contentOf are small accessors for the loosely-typed
// inbound messages.
func msgTypeOf(resp map[string]interface{}) string {
	hdr, _ := resp["header"].(map[string]interface{})
	mt, _ := hdr["msg_type"].(string)
	return mt
}

func parentIDOf(resp map[string]interface{}) string {
	ph, _ := resp["parent_header"].(map[string]interface{})
	id, _ := ph["msg_id"].(string)
	return id
}

func contentOf(resp map[string]interface{}) map[string]interface{} {
	c, _ := resp["content"].(map[string]interface{})
	return c
}

// handleColabRequest checks if a WebSocket message is a colab_request
// (e.g. request_auth for Drive mount) and auto-replies with input_reply.
// Returns true if the message was handled.
func (k *Kernel) handleColabRequest(resp map[string]interface{}) bool {
	if msgTypeOf(resp) != "colab_request" {
		return false
	}
	meta, _ := resp["metadata"].(map[string]interface{})
	colabMsgID, _ := meta["colab_msg_id"].(float64) // JSON numbers are float64

	reply := map[string]interface{}{
		"header": map[string]string{
			"msg_id":   uuid.NewString(),
			"msg_type": "input_reply",
			"session":  k.sessionID,
			"date":     time.Now().UTC().Format(time.RFC3339),
			"username": "username",
			"version":  "5.0",
		},
		"content": map[string]interface{}{
			"value": map[string]interface{}{
				"type":         "colab_reply",
				"colab_msg_id": int(colabMsgID),
			},
		},
		"channel":       "stdin",
		"metadata":      map[string]interface{}{},
		"parent_header": map[string]interface{}{},
	}
	k.conn.WriteJSON(reply)
	return true
}

// sendExecute writes an execute_request to the kernel and returns its msg_id.
func (k *Kernel) sendExecute(code string) (string, error) {
	msgID := uuid.NewString()
	msg := newJupyterMsg(msgID, k.sessionID, "execute_request", executeContent(code))
	if err := k.conn.WriteJSON(msg); err != nil {
		return "", fmt.Errorf("kernel execute write: %w", err)
	}
	return msgID, nil
}

// runCell sends code and pumps the reply loop, writing output to w until the
// kernel returns execute_reply. When printTraceback is true, an error message's
// traceback is written to w before returning; otherwise the error is returned
// without polluting w. This single loop backs both Execute and ExecuteStream.
func (k *Kernel) runCell(code string, w io.Writer, timeout time.Duration, printTraceback bool) error {
	msgID, err := k.sendExecute(code)
	if err != nil {
		return err
	}
	k.setDeadline(timeout)

	// A cell is finished only when BOTH the shell-channel execute_reply AND the
	// IOPub idle status (parented to our request) have arrived. The two channels
	// are independent through the Colab proxy, so the reply can beat trailing
	// stream output — returning on the reply alone intermittently DROPPED the
	// cell's stdout ("completed but no output"). Once either terminal message is
	// in, a short rolling grace deadline bounds the wait for the rest, and a
	// timeout past that point counts as done rather than an error.
	const grace = 3 * time.Second
	var execErr error
	gotReply, gotIdle := false, false
	for !(gotReply && gotIdle) {
		if gotReply || gotIdle {
			k.conn.SetReadDeadline(time.Now().Add(grace))
		}
		var resp map[string]interface{}
		if err := k.conn.ReadJSON(&resp); err != nil {
			if gotReply || gotIdle {
				return execErr
			}
			return fmt.Errorf("kernel execute read: %w", err)
		}
		if k.handleColabRequest(resp) {
			continue
		}
		// Only act on messages parented to OUR execute_request. The kernel's
		// IOPub stream is shared — the keepalive daemon holds a connection open,
		// other clients may be attached, and the connect handshake can leave a
		// stale reply buffered. Without this filter runCell can return on a
		// FOREIGN execute_reply and report success having run nothing (the ~1s
		// silent no-op). ExecuteDetach/Attach already filter the same way.
		if parentIDOf(resp) != msgID {
			continue
		}
		content := contentOf(resp)
		switch msgTypeOf(resp) {
		case "stream":
			if text, ok := content["text"].(string); ok {
				fmt.Fprint(w, text)
			}
		case "execute_result":
			if data, ok := content["data"].(map[string]interface{}); ok {
				if text, ok := data["text/plain"].(string); ok {
					fmt.Fprintln(w, text)
				}
			}
		case "error":
			if printTraceback {
				if tb, ok := content["traceback"].([]interface{}); ok {
					for _, line := range tb {
						if s, ok := line.(string); ok {
							fmt.Fprintln(w, StripAnsi(s))
						}
					}
				}
			}
			ename, _ := content["ename"].(string)
			evalue, _ := content["evalue"].(string)
			execErr = fmt.Errorf("kernel error %s: %s", ename, evalue)
		case "execute_reply":
			gotReply = true
			// Belt-and-suspenders: if the IOPub error message was lost, the
			// reply itself still carries the failure.
			if status, _ := content["status"].(string); status == "error" && execErr == nil {
				ename, _ := content["ename"].(string)
				evalue, _ := content["evalue"].(string)
				execErr = fmt.Errorf("kernel error %s: %s", ename, evalue)
			}
		case "status":
			if state, _ := content["execution_state"].(string); state == "idle" {
				gotIdle = true
			}
		}
	}
	return execErr
}

func (k *Kernel) setDeadline(timeout time.Duration) {
	if timeout > 0 {
		k.conn.SetReadDeadline(time.Now().Add(timeout))
	} else {
		k.conn.SetReadDeadline(time.Time{})
	}
}

// Execute sends code to the kernel and returns the collected output.
func (k *Kernel) Execute(code string, timeout time.Duration) (string, error) {
	var buf strings.Builder
	if err := k.runCell(code, &buf, timeout, false); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// ExecuteStream sends code to the kernel and streams output to w in real-time.
func (k *Kernel) ExecuteStream(code string, w io.Writer, timeout time.Duration) error {
	return k.runCell(code, w, timeout, true)
}

// ExecuteDetach submits a cell and returns as soon as the kernel CONFIRMS it has
// started executing it (the execute_input echo, or a busy status for our msg_id).
// Waiting for that confirmation is essential: WriteJSON only flushes to the
// socket buffer, so closing the connection right after a bare write races the
// server consuming the request — which is why the old fire-and-forget detach
// silently dropped cells. Once the kernel is busy on the cell it runs to
// completion regardless of the client (Jupyter semantics), keeping the runtime
// alive. Monitor via Attach or an out-of-band log.
func (k *Kernel) ExecuteDetach(code string) error {
	msgID, err := k.sendExecute(code)
	if err != nil {
		return err
	}
	k.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	defer k.conn.SetReadDeadline(time.Time{})

	for {
		var resp map[string]interface{}
		if err := k.conn.ReadJSON(&resp); err != nil {
			return fmt.Errorf("detach: confirm cell start: %w", err)
		}
		if k.handleColabRequest(resp) {
			continue
		}
		if parentIDOf(resp) != msgID {
			continue
		}
		switch msgTypeOf(resp) {
		case "execute_input":
			return nil // kernel has begun running the cell
		case "status":
			if state, _ := contentOf(resp)["execution_state"].(string); state == "busy" {
				return nil
			}
		case "error":
			ename, _ := contentOf(resp)["ename"].(string)
			evalue, _ := contentOf(resp)["evalue"].(string)
			return fmt.Errorf("kernel error %s: %s", ename, evalue)
		}
	}
}

// Attach streams all IOPub output from any running execution without sending an
// execute_request. Returns nil when the kernel goes idle (execution finished).
func (k *Kernel) Attach(w io.Writer) error {
	k.conn.SetReadDeadline(time.Time{}) // no deadline

	for {
		var resp map[string]interface{}
		if err := k.conn.ReadJSON(&resp); err != nil {
			return fmt.Errorf("attach read: %w", err)
		}
		if k.handleColabRequest(resp) {
			continue
		}
		content := contentOf(resp)
		switch msgTypeOf(resp) {
		case "stream":
			if text, ok := content["text"].(string); ok {
				fmt.Fprint(w, text)
			}
		case "execute_result":
			if data, ok := content["data"].(map[string]interface{}); ok {
				if text, ok := data["text/plain"].(string); ok {
					fmt.Fprintln(w, text)
				}
			}
		case "error":
			if tb, ok := content["traceback"].([]interface{}); ok {
				for _, line := range tb {
					if s, ok := line.(string); ok {
						fmt.Fprintln(w, StripAnsi(s))
					}
				}
			}
			ename, _ := content["ename"].(string)
			evalue, _ := content["evalue"].(string)
			return fmt.Errorf("kernel error %s: %s", ename, evalue)
		case "execute_reply":
			return nil
		case "status":
			if state, ok := content["execution_state"].(string); ok && state == "idle" {
				return nil
			}
		}
	}
}

// InterruptKernel sends an interrupt (→ KeyboardInterrupt) to the runtime's first
// kernel, stopping the currently-running cell WITHOUT restarting the kernel — so
// imports/variables survive (unlike Restart). No WebSocket needed.
func InterruptKernel(baseURL, token string) error {
	id, _, err := FirstKernelState(baseURL, token)
	if err != nil {
		return err
	}
	url := strings.TrimRight(baseURL, "/") + "/api/kernels/" + id + "/interrupt"
	req, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set(config.HProxyToken, token)
	req.Header.Set(config.HAgent, config.HAgentVal)
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("interrupt: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("interrupt failed (%d): %s", resp.StatusCode, string(body))
	}
	return nil
}

// GetStatus returns the execution_state of the kernel (busy / idle / starting).
func (k *Kernel) GetStatus() (string, error) {
	var info kernelInfo
	if err := kernelGet(k.baseURL, k.token, "/api/kernels/"+k.kernelID, &info); err != nil {
		return "", fmt.Errorf("kernel status: %w", err)
	}
	return info.ExecutionState, nil
}

// Restart restarts the kernel via the Jupyter REST API, freeing GPU memory.
// It closes the existing WebSocket and reconnects after the restart completes.
func (k *Kernel) Restart() error {
	k.conn.Close()

	url := strings.TrimRight(k.baseURL, "/") + "/api/kernels/" + k.kernelID + "/restart"
	req, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		return fmt.Errorf("kernel restart request: %w", err)
	}
	req.Header.Set(config.HProxyToken, k.token)
	req.Header.Set(config.HAgent, config.HAgentVal)

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("kernel restart: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("kernel restart failed (%d): %s", resp.StatusCode, string(body))
	}

	// Wait for the kernel to come back (poll status up to 30s).
	for i := 0; i < 30; i++ {
		time.Sleep(time.Second)
		if state, err := k.GetStatus(); err == nil && (state == "idle" || state == "starting") {
			break
		}
	}

	conn, err := dialChannel(k.baseURL, k.token, k.kernelID, k.sessionID)
	if err != nil {
		return fmt.Errorf("kernel reconnect after restart: %w", err)
	}
	k.conn = conn
	return nil
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
	// Strip .ipynb suffix if already present to avoid double extension.
	label = strings.TrimSuffix(label, ".ipynb")
	nbName := label + ".ipynb"

	// The path must contain "-jvsc-" for Colab's web UI to extract the title.
	// Format: {title}-jvsc-{id}.{ext} → Colab shows "{title}.{ext}".
	//
	// The "-jvsc-" id is DETERMINISTIC in the label (UUIDv5), not random: Jupyter
	// keys sessions by path, so a stable path makes re-creates idempotent — a
	// reconnect (or a recreate after a genuine kernel death) reuses the SAME
	// session row instead of spawning a new one. Verified on a live runtime:
	// re-POSTing an existing path returns the same session; if the kernel died
	// (Colab also drops the session), the re-POST yields a fresh live kernel
	// under the same path. Either way the "Active Sessions" panel shows one row
	// per label, not a growing pile.
	jvscID := uuid.NewSHA1(uuid.NameSpaceURL, []byte("colab-tui:"+label)).String()
	sessionPath := label + "-jvsc-" + jvscID + ".ipynb"

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

	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(baseURL, "/")+"/api/sessions", bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("create session request: %w", err)
	}
	req.Header.Set(config.HProxyToken, token)
	req.Header.Set(config.HAgent, config.HAgentVal)
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
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
