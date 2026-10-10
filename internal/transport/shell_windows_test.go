//go:build windows

package transport

import (
	"context"
	"strings"
	"testing"
)

// Quoted arguments reach the program intact (cmd.exe parses the line itself).
func TestLocalQuotesOnWindows(t *testing.T) {
	out, err := (&Local{}).Run(context.Background(), `if "a b?c=d"=="a b?c=d" (echo same) else (echo differ)`, nil)
	if err != nil || strings.TrimSpace(out) != "same" {
		t.Fatalf("%q %v", out, err)
	}
}
