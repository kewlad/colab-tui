package runtime

import (
	"testing"
)

func TestStripAnsi(t *testing.T) {
	input := "\x1b[32mhello\x1b[0m"
	want := "hello"
	got := StripAnsi(input)
	if got != want {
		t.Errorf("StripAnsi(%q) = %q; want %q", input, got, want)
	}
}

func TestStripAnsiNoEscape(t *testing.T) {
	input := "hello world"
	got := StripAnsi(input)
	if got != input {
		t.Errorf("StripAnsi(%q) = %q; want %q", input, got, input)
	}
}

func TestCleanOutput(t *testing.T) {
	raw := "root@vm:~# echo hi\nhi\nDONE_abc\nroot@vm:~#"
	got := CleanOutput(raw, "echo hi", "DONE_abc")
	want := "hi"
	if got != want {
		t.Errorf("CleanOutput() = %q; want %q", got, want)
	}
}
