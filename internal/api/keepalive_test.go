package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
)

// The Tunnel Frontend stamps the assignment's last-active time on arrival and
// only then forwards to the VM, which often never answers — so a read timeout
// means the keep-alive LANDED. Reporting it as an error would make the daemon
// log failures for a runtime it is successfully keeping alive.
func TestKeepaliveResult(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		wantErr bool
	}{
		{"success", nil, false},
		{"context deadline", context.DeadlineExceeded, false},
		{"read timeout", &net.OpError{Err: timeoutErr{}}, false},
		{"wrapped timeout", fmt.Errorf("do request: %w", &net.OpError{Err: timeoutErr{}}), false},
		// A deleted assignment (404) must NOT be swallowed — that is how the
		// daemon learns the runtime is gone.
		{"http rejection", errors.New("API error 404: assignment not found"), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := keepaliveResult(tc.err); (got != nil) != tc.wantErr {
				t.Errorf("keepaliveResult(%v) = %v, wantErr=%v", tc.err, got, tc.wantErr)
			}
		})
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }
