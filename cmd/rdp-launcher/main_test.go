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
		name   string
		args   []string
		mode   string
		action string
		params string
	}{
		{"run", []string{"run", "--params", "/new", "explorer"}, controller.ModeRemoteApp, "run", "/separate /new"},
		{"open", []string{"open", "--app", "文件资源管理器", `J:\folder with spaces\`}, controller.ModeRemoteApp, "run", `/separate "J:\folder with spaces\\"`},
		{"desktop", []string{"desktop"}, controller.ModeDesktop, "connect", ""},
		{"remoteapp", []string{"remoteapp"}, controller.ModeRemoteApp, "connect", ""},
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
			received := serveHostOnce(t, listener, nil)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			var out, stderr bytes.Buffer
			args := append([]string{"--config", path, "--socket", socket}, tc.args...)
			if err := run(ctx, args, &out, &stderr); err != nil {
				t.Fatal(err, stderr.String())
			}
			req := <-received
			if req.Action != "request" {
				t.Fatalf("wrong envelope: %+v", req)
			}
			got := req.Request
			if got.Action != tc.action || !got.UserContext || got.Mode != tc.mode || got.Batch != nil || got.Choice != "" {
				t.Fatalf("wrong request: %+v", got)
			}
			switch tc.action {
			case "connect":
				if got.Target != "" {
					t.Fatalf("connect rewrote a business target: %+v", got)
				}
			case "run":
				if got.Target != `C:\Windows\explorer.exe` || got.Params != tc.params || got.WorkingDir != `C:\Windows` {
					t.Fatalf("wrong application payload: %+v", got)
				}
			}
		})
	}
}

func TestUserQueriesAreModeNeutralAndUserContext(t *testing.T) {
	for _, tc := range []struct {
		name      string
		command   string
		action    string
		withIcons bool
	}{
		{"apps", "apps", "list_apps", false},
		{"sync-apps", "sync-apps", "list_apps", true},
		{"mounts", "mounts", "get_mapped_drives", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
			t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
			path := filepath.Join(root, "config.toml")
			if err := os.WriteFile(path, []byte("# fixture\n"), 0600); err != nil {
				t.Fatal(err)
			}
			socket := filepath.Join(root, "controller.sock")
			listener, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			received := serveHostOnce(t, listener, func(_ *json.Decoder, encoder *json.Encoder, _ host.Request) {
				_ = encoder.Encode(host.Response{Type: "result", Data: json.RawMessage(`{"protocol":2,"status":"ok","apps":[]}`)})
			})
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			var out, stderr bytes.Buffer
			if err = run(ctx, []string{"--config", path, "--socket", socket, tc.command}, &out, &stderr); err != nil {
				t.Fatal(err, stderr.String())
			}
			request := <-received
			got := request.Request
			// Read-only user queries must not imply a display mode or a reconnect.
			if request.Action != "request" || got.Action != tc.action || !got.UserContext || got.Mode != "" || got.WithIcons != tc.withIcons {
				t.Fatalf("wrong query request: %+v", request)
			}
		})
	}
	t.Run("ping", func(t *testing.T) {
		root := t.TempDir()
		socket := filepath.Join(root, "controller.sock")
		listener, err := net.Listen("unix", socket)
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		received := serveHostOnce(t, listener, nil)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		var out, stderr bytes.Buffer
		if err = run(ctx, []string{"--socket", socket, "ping"}, &out, &stderr); err != nil {
			t.Fatal(err, stderr.String())
		}
		request := <-received
		if request.Request.Action != "ping" || request.Request.UserContext || request.Request.Mode != "" {
			t.Fatalf("ping is not backend-only: %+v", request.Request)
		}
	})
}

func TestOpenBatchIsOneRequestAndOneConfirmation(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	path := filepath.Join(root, "config.toml")
	if err := os.WriteFile(path, []byte(`[[apps]]
id="explorer"
name="Explorer"
target='C:\Windows\explorer.exe'
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
	confirmed := make(chan controller.SwitchDecision, 2)
	received := serveHostOnce(t, listener, func(decoder *json.Decoder, encoder *json.Encoder, request host.Request) {
		if request.Action != "request" || request.Request.Action != "batch" {
			t.Errorf("wrong batch envelope: %+v", request)
			return
		}
		if !request.Request.UserContext || request.Request.Mode != controller.ModeRemoteApp || request.Request.Choice != "" {
			t.Errorf("wrong batch mode/choice: %+v", request.Request)
		}
		batch := request.Request.Batch
		if len(batch) != 2 {
			t.Errorf("wrong ordered batch: %+v", batch)
			return
		}
		for index, name := range []string{`J:\a.txt`, `J:\b.txt`} {
			item := batch[index]
			if item.Action != "run" || item.Target != `C:\Windows\explorer.exe` || !strings.Contains(item.Params, name) || item.WorkingDir != `C:\Windows` {
				t.Errorf("batch item %d lost its payload: %+v", index, item)
			}
			if item.Batch != nil || item.Choice != "" || item.Confirm != nil || item.Mode != "" || item.UserContext {
				t.Errorf("batch item is not plain business payload: %+v", item)
			}
		}
		prompt := controller.SwitchPrompt{TargetMode: controller.ModeRemoteApp, Allowed: []controller.SwitchDecision{controller.SwitchKeep, controller.SwitchCancel}}
		if err := encoder.Encode(host.Response{Type: "confirm", ID: 1, Prompt: &prompt}); err != nil {
			t.Error(err)
			return
		}
		var reply struct {
			Type     string
			ID       uint64
			Decision controller.SwitchDecision
		}
		if err := decoder.Decode(&reply); err != nil {
			t.Error(err)
			return
		}
		if reply.Type != "decision" || reply.ID != 1 {
			t.Errorf("wrong decision frame: %+v", reply)
		}
		confirmed <- reply.Decision
		_ = encoder.Encode(host.Response{Type: "result", Data: json.RawMessage(`[{"status":"ok"},{"status":"ok"}]`)})
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	prompts := 0
	frontend := confirmFrontend{
		stdin:   strings.NewReader(""),
		stderr:  &bytes.Buffer{},
		wayland: func() bool { return true },
		gui: func(context.Context, controller.SwitchPrompt) (controller.SwitchDecision, error) {
			prompts++
			return controller.SwitchKeep, nil
		},
	}
	var out, stderr bytes.Buffer
	if err = runWith(ctx, []string{"--config", path, "--socket", socket, "open", "--app", "explorer", `J:\a.txt`, `J:\b.txt`}, &out, &stderr, frontend); err != nil {
		t.Fatal(err, stderr.String())
	}
	request := <-received
	if request.Request.Choice != "" {
		t.Fatalf("implicit choice was sent: %+v", request.Request)
	}
	if decision := <-confirmed; decision != controller.SwitchKeep {
		t.Fatal(decision)
	}
	if prompts != 1 {
		t.Fatalf("prompted %d times for one batch", prompts)
	}
	if !strings.Contains(out.String(), `"status": "ok"`) {
		t.Fatal(out.String())
	}
}

func TestOpenExplicitChoiceNeedsNoPrompt(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	path := filepath.Join(root, "config.toml")
	if err := os.WriteFile(path, []byte("# no apps\n"), 0600); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(root, "controller.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	received := serveHostOnce(t, listener, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	frontend := confirmFrontend{
		stdin:   strings.NewReader(""),
		stderr:  &bytes.Buffer{},
		wayland: func() bool { return false },
		gui: func(context.Context, controller.SwitchPrompt) (controller.SwitchDecision, error) {
			t.Error("explicit choice still prompted")
			return controller.SwitchCancel, nil
		},
	}
	var out, stderr bytes.Buffer
	if err = runWith(ctx, []string{"--on-conflict=logoff", "--config", path, "--socket", socket, "open", `J:\a.txt`}, &out, &stderr, frontend); err != nil {
		t.Fatal(err, stderr.String())
	}
	request := <-received
	if request.Request.Choice != controller.SwitchLogoff || request.Request.Action != "open" || !request.Request.UserContext || request.Request.Mode != controller.ModeRemoteApp {
		t.Fatalf("explicit choice was not sent once: %+v", request.Request)
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

func TestOnConflictIsRejectedWhereNoSessionChoiceExists(t *testing.T) {
	for _, args := range [][]string{
		{"--on-conflict=keep", "ping"},
		{"--on-conflict=keep", "status"},
		{"--on-conflict=maybe", "open", `J:\a.txt`},
	} {
		var out, stderr bytes.Buffer
		err := run(context.Background(), args, &out, &stderr)
		if err == nil {
			t.Fatal("accepted", args)
		}
	}
}
