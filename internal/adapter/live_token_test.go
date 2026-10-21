package adapter

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestLiveTokenFileMatchesWindowsLineEndingRules(t *testing.T) {
	for _, tc := range []struct {
		name, data, want string
		invalid          bool
	}{
		{"raw", "token", "token", false},
		{"LF", "token\n", "token", false},
		{"CRLF", "token\r\n", "token", false},
		{"spaces preserved", " token \t\r\n", " token \t", false},
		{"empty", "", "", true},
		{"empty line", "\r\n", "", true},
		{"bare CR", "token\r", "", true},
		{"two endings", "token\n\n", "", true},
		{"embedded newline", "one\ntwo\n", "", true},
		{"invalid UTF-8", "\xff\n", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "token")
			if err := os.WriteFile(path, []byte(tc.data), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("RDP_LIVE_TOKEN_FILE", path)
			got, err := liveAgent("unused").token(context.Background())
			if tc.invalid {
				if err == nil {
					t.Fatal("invalid token accepted")
				}
			} else if err != nil || got != tc.want {
				t.Fatal("token parsing differs from Windows", err)
			}
		})
	}
}
