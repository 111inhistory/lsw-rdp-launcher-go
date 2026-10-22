package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/controller"
	"github.com/111inhistory/lsw-rdp-launcher-go/internal/host"
)

func TestInstallShortcutsWithoutControllerOrGuest(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	config := filepath.Join(root, "config.toml")
	if err := os.WriteFile(config, []byte("# Local-only installation fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	args := []string{"--config", config, "--socket", filepath.Join(root, "absent.sock"), "install-shortcuts"}
	if err := run(context.Background(), args, &out, &stderr); err != nil {
		t.Fatal(err, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(root, "data", "applications", "rdp-launcher-go.desktop")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Windows 桌面") {
		t.Fatal(out.String())
	}
	if err := run(context.Background(), append(args, "unexpected"), &out, &stderr); err == nil {
		t.Fatal("unexpected argument accepted")
	}
}

func TestApplicationAndDesktopCLIRequests(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		params  string
		desktop bool
	}{
		{"run", []string{"run", "--params", "/new", "explorer"}, "/separate /new", false},
		{"open", []string{"open", "--app", "文件资源管理器", `J:\folder with spaces\`}, `/separate "J:\folder with spaces\\"`, false},
		{"desktop", []string{"desktop"}, "", true},
		{"remoteapp", []string{"remoteapp"}, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
			path := filepath.Join(root, "config.toml")
			if err := os.WriteFile(path, []byte(`[[apps]]
id="explorer"
name="文件资源管理器"
target='C:\Windows\explorer.exe'
arguments='/separate'
working_dir='C:\Windows'
`), 0600); err != nil {
				t.Fatal(err)
			}
			socket := filepath.Join(root, "controller.sock")
			listener, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			received := make(chan host.Request, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				var req host.Request
				if json.NewDecoder(conn).Decode(&req) != nil {
					return
				}
				received <- req
				_ = json.NewEncoder(conn).Encode(host.Response{Data: json.RawMessage(`{"status":"ok"}`)})
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			var out, stderr bytes.Buffer
			args := append([]string{"--config", path, "--socket", socket}, tc.args...)
			if err := run(ctx, args, &out, &stderr); err != nil {
				t.Fatal(err, stderr.String())
			}
			req := <-received
			if tc.desktop {
				need := controller.Desktop
				if tc.name == "remoteapp" {
					need = controller.Interactive
				}
				if req.Action != "switch-display" || req.Request.Action != "connect" || req.Request.Need != need || req.Request.SwitchDisplay || req.Request.Target != "" {
					t.Fatalf("wrong desktop request: %+v", req)
				}
			} else if req.Action != "request" || req.Request.Action != "run" || req.Request.Target != `C:\Windows\explorer.exe` || req.Request.Params != tc.params || req.Request.WorkingDir != `C:\Windows` {
				t.Fatalf("wrong request: %+v", req)
			}
		})
	}
}

func TestAuthAgentSetCheckClearNeverPrintsToken(t *testing.T) {
	root := t.TempDir()
	tool := filepath.Join(root, "secret-tool")
	script := `#!/bin/sh
printf '%s\n' "$@" > "$FAKE_ARGS_FILE"
case "$1" in
  lookup) cat "$FAKE_SECRET_FILE" ;;
  store) cat > "$FAKE_STORE_FILE" ;;
esac
`
	if err := os.WriteFile(tool, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	argsPath := filepath.Join(root, "arguments")
	storePath := filepath.Join(root, "stored")
	secretPath := filepath.Join(root, "secret")
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_ARGS_FILE", argsPath)
	t.Setenv("FAKE_STORE_FILE", storePath)
	t.Setenv("FAKE_SECRET_FILE", secretPath)

	configPath := filepath.Join(root, "config.toml")
	config := `[server]
service = "rdp-bridge"
agent_service = "rdp-bridge-agent"
user = "alice"
`
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := host.LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if err = auth(context.Background(), c, []string{"agent", "set"}, strings.NewReader("shared-token"), &out, &stderr); err != nil {
		t.Fatal(err, stderr.String())
	}
	if strings.Contains(out.String(), "shared-token") || strings.Contains(stderr.String(), "shared-token") {
		t.Fatal("token was printed")
	}
	stored, err := os.ReadFile(storePath)
	if err != nil || string(stored) != "shared-token" {
		t.Fatalf("stored token = %q, %v", stored, err)
	}
	arguments, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(arguments); !strings.Contains(got, "store") || !strings.Contains(got, "rdp-bridge-agent") || !strings.Contains(got, "\nagent\n") {
		t.Fatalf("agent set arguments = %q", got)
	}

	if err = os.WriteFile(secretPath, []byte("shared-token"), 0600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err = auth(context.Background(), c, []string{"agent", "check"}, strings.NewReader(""), &out, &stderr); err != nil {
		t.Fatal(err)
	}
	if out.String() != "Agent token keyring entry exists\n" || strings.Contains(out.String(), "shared-token") {
		t.Fatalf("check output = %q", out.String())
	}

	out.Reset()
	if err = auth(context.Background(), c, []string{"agent", "clear"}, strings.NewReader(""), &out, &stderr); err != nil {
		t.Fatal(err)
	}
	arguments, err = os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(arguments); !strings.HasPrefix(got, "clear\n") || !strings.Contains(got, "rdp-bridge-agent") {
		t.Fatalf("agent clear arguments = %q", got)
	}

	if err = auth(context.Background(), c, []string{"agent"}, strings.NewReader(""), &out, &stderr); err == nil {
		t.Fatal("accepted auth agent without an action")
	}

	for _, invalid := range []string{"", "token\n", "tok\r\nen"} {
		out.Reset()
		if err = auth(context.Background(), c, []string{"agent", "set"}, strings.NewReader(invalid), &out, &stderr); err == nil {
			t.Fatalf("stored invalid piped secret %q", invalid)
		}
		if strings.Contains(out.String(), invalid) && invalid != "" {
			t.Fatal("invalid secret was printed")
		}
	}
}
