package main

import (
	"strings"
	"testing"
)

func TestFormatRAM(t *testing.T) {
	if got := formatRAM("4456448 13312000"); got != "4.2/12.7 GB used (33%)" {
		t.Fatalf("formatRAM: got %q", got)
	}
	if got := formatRAM(""); got != "" {
		t.Fatalf("missing RAM probe must yield empty string, got %q", got)
	}
}

func TestMemoryWarnings(t *testing.T) {
	// Healthy: no warnings.
	if w := memoryWarnings("4456448 13312000", "Tesla T4, 6617 MiB, 15360 MiB"); len(w) != 0 {
		t.Fatalf("healthy runtime produced warnings: %v", w)
	}
	// RAM at 95%: warn (the OOM-kills-the-VM failure mode).
	w := memoryWarnings("12646400 13312000", "Tesla T4, 6617 MiB, 15360 MiB")
	if len(w) != 1 || !strings.Contains(w[0], "RAM 95% used") {
		t.Fatalf("RAM pressure not flagged: %v", w)
	}
	// GPU memory at 94%: warn.
	w = memoryWarnings("4456448 13312000", "Tesla T4, 14500 MiB, 15360 MiB")
	if len(w) != 1 || !strings.Contains(w[0], "GPU memory 94% used") {
		t.Fatalf("GPU pressure not flagged: %v", w)
	}
	// No GPU: only the probe placeholder — must not warn or panic.
	if w := memoryWarnings("4456448 13312000", "(no GPU)"); len(w) != 0 {
		t.Fatalf("no-GPU runtime produced warnings: %v", w)
	}
}
