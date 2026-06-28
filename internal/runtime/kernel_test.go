package runtime

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
