package runtime

import (
	"regexp"
	"strings"
)

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

// StripAnsi removes ANSI escape sequences from s.
func StripAnsi(s string) string {
	return ansiRe.ReplaceAllString(s, "")
}

// CleanOutput strips the echoed command line, the marker line, and the trailing
// shell prompt from raw TTY output, returning only the command's output.
func CleanOutput(raw, cmd, marker string) string {
	clean := StripAnsi(raw)
	lines := strings.Split(clean, "\n")
	var out []string
	for _, line := range lines {
		stripped := strings.TrimRight(line, "\r")
		// Skip the echoed command line.
		if strings.Contains(stripped, cmd) {
			continue
		}
		// Stop at (and skip) the marker line.
		if strings.Contains(stripped, marker) {
			break
		}
		out = append(out, stripped)
	}
	// Trim trailing empty lines / prompt fragments.
	result := strings.Join(out, "\n")
	return strings.TrimRight(result, "\r\n ")
}
