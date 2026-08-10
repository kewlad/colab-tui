package main

import (
	"errors"
	"testing"

	"github.com/netanelbi/colab-tui/internal/runtime"
)

func TestSplitRC(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantOut string
		wantRC  int
	}{
		{"success", "hello\n" + rcMarker + "0", "hello", 0},
		{"failure", "boom\n" + rcMarker + "7", "boom", 7},
		{"no output", "\n" + rcMarker + "1", "", 1},
		// The cell raised before printing the marker — never guess 0.
		{"missing marker", "traceback...", "traceback...", runtime.StatusUnknown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, rc := splitRC(tc.in)
			if out != tc.wantOut || rc != tc.wantRC {
				t.Errorf("splitRC(%q) = (%q, %d), want (%q, %d)", tc.in, out, rc, tc.wantOut, tc.wantRC)
			}
		})
	}
}

// An unreadable status must not be reported as a failure (it would break every
// caller), but it must also not be silently reported as success.
func TestReportStatus(t *testing.T) {
	if err := reportStatus(0, "command"); err != nil {
		t.Errorf("rc=0 should succeed, got %v", err)
	}
	if err := reportStatus(runtime.StatusUnknown, "command"); err != nil {
		t.Errorf("unknown status should not fail the command, got %v", err)
	}
	err := reportStatus(7, "command")
	var ec exitCode
	if !errors.As(err, &ec) || int(ec) != 7 {
		t.Errorf("rc=7 should yield exitCode(7), got %v", err)
	}
}
