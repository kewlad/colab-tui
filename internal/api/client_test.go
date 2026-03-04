package api

import (
	"strings"
	"testing"
)

func TestMakeNBH(t *testing.T) {
	input := "550e8400-e29b-41d4-a716-446655440000"
	result := MakeNBH(input)

	if len(result) != 44 {
		t.Errorf("expected length 44, got %d", len(result))
	}
	if strings.Contains(result, "-") {
		t.Error("result should not contain dashes")
	}
	// Verify the UUID portion is correct (dashes replaced with underscores)
	noDash := strings.ReplaceAll(input, "-", "_")
	if !strings.HasPrefix(result, noDash) {
		t.Errorf("result %q should start with %q", result, noDash)
	}
	// Verify padding with dots
	if !strings.HasSuffix(result, ".") {
		t.Error("result should be padded with dots")
	}
}

func TestStripXSS(t *testing.T) {
	input := ")]}'\n{\"a\":1}"
	result := StripXSS(input)
	if result != `{"a":1}` {
		t.Errorf("expected {\"a\":1}, got %q", result)
	}
}

func TestStripXSSNoPrefix(t *testing.T) {
	input := `{"a":1}`
	result := StripXSS(input)
	if result != input {
		t.Errorf("expected input unchanged, got %q", result)
	}
}
