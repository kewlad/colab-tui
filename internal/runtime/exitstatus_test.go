package runtime

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

// The end marker must carry $? — without it every exec exits 0 and cannot be
// used as a shell condition.
func TestWrapCommandCarriesExitStatus(t *testing.T) {
	got := wrapCommand("START_x", "false", "END_x")
	if !strings.Contains(got, "echo END_x:$?") {
		t.Fatalf("end marker does not carry $?: %q", got)
	}
}

func TestParseExitStatus(t *testing.T) {
	const end = "END_abc"
	tests := []struct {
		name string
		in   string
		want int
	}{
		// The echoed command line contains the marker followed by the literal
		// ":$?" — it must never be mistaken for a status.
		{"echo only", "echo START_abc; bash -c \"...\"; echo END_abc:$?\n", StatusUnknown},
		{"success", "echo END_abc:$?\nout\nEND_abc:0\n", 0},
		{"failure", "echo END_abc:$?\nout\nEND_abc:7\n", 7},
		{"multi digit", "echo END_abc:$?\nEND_abc:130\n", 130},
		{"truncated digits", "echo END_abc:$?\nEND_abc:", StatusUnknown},
		{"no marker", "just output\n", StatusUnknown},
		// Scrollback from a previous command must not win over this one.
		{"last wins", "END_abc:0\nEND_abc:3\n", 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseExitStatus(tc.in, end); got != tc.want {
				t.Errorf("parseExitStatus(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// extractBetweenMarkers must not leak the ":N" status suffix into the output.
func TestExtractBetweenMarkersDropsStatusSuffix(t *testing.T) {
	raw := "echo START_a; bash -c \"...\"; echo END_a:$?\nSTART_a\nhello\nEND_a:0\n"
	got := extractBetweenMarkers(raw, "START_a", "END_a", "cmd")
	if strings.Contains(got, "END_a") || strings.Contains(got, ":0") {
		t.Fatalf("status suffix leaked into output: %q", got)
	}
	if strings.TrimSpace(got) != "hello" {
		t.Fatalf("output = %q, want %q", got, "hello")
	}
}

// A 404 means the runtime image has no sshd — the message must say the runtime
// must be recreated, not send the user hunting for a local misconfiguration.
var errBadHandshake = errors.New("websocket: bad handshake")

func TestSSHDialErrorMessages(t *testing.T) {
	tests := []struct {
		status int
		want   string
	}{
		{http.StatusBadRequest, "ssh-ed25519"},
		{http.StatusUnauthorized, "colab resume"},
		{http.StatusNotFound, "colab start"},
		{http.StatusTooManyRequests, "already attached"},
		{http.StatusBadGateway, "sshd is unreachable"},
	}
	for _, tc := range tests {
		err := sshDialError(errBadHandshake, &http.Response{StatusCode: tc.status})
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("status %d: %q does not mention %q", tc.status, err, tc.want)
		}
	}
	// No response at all is a network failure, not a server rejection.
	if err := sshDialError(errBadHandshake, nil); !strings.Contains(err.Error(), "network") {
		t.Errorf("nil response: %q does not mention the network", err)
	}
}
