package runtime

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// withTestClient points the package httpClient at a test server for the duration
// of the test, restoring it afterward.
func withTestClient(t *testing.T, srv *httptest.Server) {
	t.Helper()
	prev := httpClient
	httpClient = srv.Client()
	t.Cleanup(func() { httpClient = prev })
}

// TestFirstKernelState_NoKernels verifies the load-bearing distinction behind the
// "1 session per machine unless it died" invariant: a reachable runtime reporting
// an EMPTY kernel list returns ErrNoKernels (so NewKernel may create one), while a
// transient/garbage response returns a plain error (so NewKernel reuses instead of
// spawning a duplicate session).
func TestFirstKernelState_NoKernels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	withTestClient(t, srv)

	_, _, err := FirstKernelState(srv.URL, "tok")
	if !errors.Is(err, ErrNoKernels) {
		t.Fatalf("empty kernel list: got %v, want ErrNoKernels", err)
	}
}

func TestFirstKernelState_TransientErrorIsNotNoKernels(t *testing.T) {
	// A 502-style HTML error page (what a flaky proxy returns) must NOT be read
	// as "no kernels" — that would let a reconnect blip mint a duplicate session.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`<html>502 Bad Gateway</html>`))
	}))
	defer srv.Close()
	withTestClient(t, srv)

	_, _, err := FirstKernelState(srv.URL, "tok")
	if err == nil {
		t.Fatal("transient error: got nil, want non-nil")
	}
	if errors.Is(err, ErrNoKernels) {
		t.Fatalf("transient error must not be ErrNoKernels: got %v", err)
	}
}

// captureCreateSessionPath POSTs to a fake /api/sessions, returning the path the
// client sent so we can assert it is stable per label.
func captureCreateSessionPath(t *testing.T, label string) string {
	t.Helper()
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal(body, &req)
		gotPath = req.Path
		_, _ = w.Write([]byte(`{"kernel":{"id":"k1"}}`))
	}))
	defer srv.Close()
	withTestClient(t, srv)

	if _, err := CreateSession(srv.URL, "tok", label); err != nil {
		t.Fatalf("CreateSession(%q): %v", label, err)
	}
	return gotPath
}

// TestCreateSession_DeterministicPath is the belt-and-suspenders half of "no spam
// even in rare occasions": the session path is a pure function of the label, so a
// recreate (after a genuine death) reuses the same Jupyter session row instead of
// minting a new one. Different labels get different rows.
func TestCreateSession_DeterministicPath(t *testing.T) {
	p1 := captureCreateSessionPath(t, "v16a")
	p2 := captureCreateSessionPath(t, "v16a")
	if p1 != p2 {
		t.Fatalf("same label produced different paths: %q vs %q", p1, p2)
	}
	if !strings.HasPrefix(p1, "v16a-jvsc-") || !strings.HasSuffix(p1, ".ipynb") {
		t.Fatalf("unexpected path shape: %q (want v16a-jvsc-<id>.ipynb)", p1)
	}
	if other := captureCreateSessionPath(t, "v16b"); other == p1 {
		t.Fatalf("different labels collided on path %q", p1)
	}
}

// wsKernel spawns a fake kernel-channel websocket server. handler receives the
// connection after the client's first message (the execute_request) has been
// read, along with that request's msg_id.
func wsKernel(t *testing.T, handler func(conn *websocket.Conn, msgID string)) *Kernel {
	t.Helper()
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var req map[string]interface{}
		if err := conn.ReadJSON(&req); err != nil {
			return
		}
		hdr, _ := req["header"].(map[string]interface{})
		msgID, _ := hdr["msg_id"].(string)
		handler(conn, msgID)
	}))
	t.Cleanup(srv.Close)

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial test ws: %v", err)
	}
	k := &Kernel{conn: conn, kernelID: "k1", sessionID: "s1", baseURL: srv.URL, token: "tok"}
	t.Cleanup(func() { k.Close() })
	return k
}

// iopubMsg builds a minimal inbound kernel message parented to msgID.
func iopubMsg(msgType, parentID string, content map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"header":        map[string]interface{}{"msg_type": msgType},
		"parent_header": map[string]interface{}{"msg_id": parentID},
		"content":       content,
	}
}

// TestExecute_CapturesStreamArrivingAfterReply is the regression test for the
// "cell completed but printed nothing" bug: the shell-channel execute_reply and
// the IOPub stream are independent through the Colab proxy, so the reply can
// arrive FIRST. runCell must keep reading until idle instead of returning on the
// reply and dropping the output.
func TestExecute_CapturesStreamArrivingAfterReply(t *testing.T) {
	k := wsKernel(t, func(conn *websocket.Conn, msgID string) {
		_ = conn.WriteJSON(iopubMsg("execute_reply", msgID, map[string]interface{}{"status": "ok"}))
		_ = conn.WriteJSON(iopubMsg("stream", msgID, map[string]interface{}{"name": "stdout", "text": "hello\n"}))
		_ = conn.WriteJSON(iopubMsg("status", msgID, map[string]interface{}{"execution_state": "idle"}))
	})
	out, err := k.Execute("print('hello')", 5*time.Second)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out != "hello\n" {
		t.Fatalf("got output %q, want %q (stream after reply was dropped)", out, "hello\n")
	}
}

// TestExecute_ReplyWithoutIdleFinishesViaGrace: if the idle status never arrives
// (lost through the proxy), a completed cell must still return successfully after
// the short grace window — not surface a read-timeout error.
func TestExecute_ReplyWithoutIdleFinishesViaGrace(t *testing.T) {
	k := wsKernel(t, func(conn *websocket.Conn, msgID string) {
		_ = conn.WriteJSON(iopubMsg("stream", msgID, map[string]interface{}{"name": "stdout", "text": "done\n"}))
		_ = conn.WriteJSON(iopubMsg("execute_reply", msgID, map[string]interface{}{"status": "ok"}))
		time.Sleep(10 * time.Second) // never send idle; hold the conn open
	})
	out, err := k.Execute("print('done')", 30*time.Second)
	if err != nil {
		t.Fatalf("Execute: %v (a lost idle must not fail a completed cell)", err)
	}
	if out != "done\n" {
		t.Fatalf("got output %q, want %q", out, "done\n")
	}
}

// TestExecute_IgnoresForeignMessages re-pins the parent-msg-id filter: replies
// and streams from OTHER clients on the shared kernel must not terminate or
// pollute our cell.
func TestExecute_IgnoresForeignMessages(t *testing.T) {
	k := wsKernel(t, func(conn *websocket.Conn, msgID string) {
		_ = conn.WriteJSON(iopubMsg("execute_reply", "someone-else", map[string]interface{}{"status": "ok"}))
		_ = conn.WriteJSON(iopubMsg("stream", "someone-else", map[string]interface{}{"text": "noise\n"}))
		_ = conn.WriteJSON(iopubMsg("stream", msgID, map[string]interface{}{"text": "mine\n"}))
		_ = conn.WriteJSON(iopubMsg("execute_reply", msgID, map[string]interface{}{"status": "ok"}))
		_ = conn.WriteJSON(iopubMsg("status", msgID, map[string]interface{}{"execution_state": "idle"}))
	})
	out, err := k.Execute("print('mine')", 5*time.Second)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out != "mine\n" {
		t.Fatalf("got output %q, want %q", out, "mine\n")
	}
}

func TestFirstKernelState_ReturnsFirstKernel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"id":"abc","execution_state":"idle"}]`))
	}))
	defer srv.Close()
	withTestClient(t, srv)

	id, state, err := FirstKernelState(srv.URL, "tok")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "abc" || state != "idle" {
		t.Fatalf("got id=%q state=%q, want abc/idle", id, state)
	}
}
