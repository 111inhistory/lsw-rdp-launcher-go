package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/controller"
)

type processCapture struct {
	Args  []string
	Input string
	Env   []string
}

func TestMain(m *testing.M) {
	if os.Getenv("RDP_TEST_CHILD") == "1" {
		if os.Getenv("RDP_TEST_IGNORE_TERM") == "1" {
			signal.Ignore(syscall.SIGTERM)
		}
		input, err := io.ReadAll(os.Stdin)
		if err != nil {
			os.Exit(2)
		}
		_ = json.NewEncoder(os.Stdout).Encode(processCapture{os.Args[1:], string(input), os.Environ()})
		for {
			time.Sleep(time.Hour)
		}
	}
	os.Exit(m.Run())
}
func displayTestConfig() DisplayConfig {
	return DisplayConfig{Binary: os.Args[0], Host: "192.0.2.10", User: "test-user", Program: `C:\Program Files\RemoteApp\agent.exe`, VideoDriver: "wayland", ScaleDesktop: 200, Clipboard: true, IgnoreCertificate: true, Password: func(context.Context) (string, error) { return "test-only-password", nil }}
}
func TestInteractiveReadinessRequiresActiveSession(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprint(legacy), func(t *testing.T) {
			t.Setenv("RDP_TEST_CHILD", "1")
			var probes, tokenReads atomic.Int32
			a := jsonAgent(t, func(req agentRequest) any {
				if req.Action != "status" {
					t.Error(req.Action)
				}
				if req.Token != "shared-token" {
					t.Errorf("probe token = %q", req.Token)
				}
				n := probes.Add(1)
				if legacy {
					return map[string]any{"status": "ok"}
				}
				return map[string]any{"status": "ok", "session_id": 2, "session_active": n > 1}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			a.AuthToken = func(context.Context) (string, error) {
				tokenReads.Add(1)
				return "shared-token", nil
			}
			cfg := displayTestConfig()
			cfg.Log = io.Discard
			d := NewDisplay(ctx, cfg, a, func(context.Context, controller.Event) error { return nil })
			err := d.Start(ctx, 1, false)
			if legacy {
				if err == nil || !strings.Contains(err.Error(), "update the Windows agent") {
					t.Fatal(err)
				}
			} else {
				if err != nil || probes.Load() < 2 {
					t.Fatal(err, probes.Load())
				}
			}
			if tokenReads.Load() != 1 {
				t.Fatalf("token provider called %d times during startup probes", tokenReads.Load())
			}
			if err := d.Stop(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFreeRDPArgumentsStdinAndOwnedExit(t *testing.T) {
	for _, tc := range []struct {
		name                string
		ignoreTerm, desktop bool
	}{{"RemoteApp-SIGTERM", false, false}, {"RemoteApp-stop-timeout", true, false}, {"Desktop-no-agent", false, true}} {
		ignoreTerm := tc.ignoreTerm
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("RDP_TEST_CHILD", "1")
			if ignoreTerm {
				t.Setenv("RDP_TEST_IGNORE_TERM", "1")
			}
			t.Setenv("HTTP_PROXY", "http://invalid-proxy")
			t.Setenv("SDL_VIDEODRIVER", "x11")
			a := Agent{}
			if !tc.desktop {
				a = jsonAgent(t, func(req agentRequest) any {
					return map[string]any{"status": "ok", "session_id": 2, "session_active": true}
				})
			}
			root, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			reader, writer := io.Pipe()
			defer reader.Close()
			defer writer.Close()
			captured := make(chan processCapture, 1)
			go func() { var capture processCapture; _ = json.NewDecoder(reader).Decode(&capture); captured <- capture }()
			events := make(chan controller.DisplayExited, 1)
			config := displayTestConfig()
			config.Log = writer
			d := NewDisplay(root, config, a, func(_ context.Context, event controller.Event) error {
				events <- event.(controller.DisplayExited)
				return nil
			})
			operation, cancelOperation := context.WithCancel(root)
			if err := d.Start(operation, 42, tc.desktop); err != nil {
				t.Fatal(err)
			}
			cancelOperation() // A completed operation does not own the display lifetime.
			var capture processCapture
			select {
			case capture = <-captured:
			case <-root.Done():
				t.Fatal(root.Err())
			}
			args := strings.Join(capture.Args, " ")
			wantArgs := []string{"/v:192.0.2.10", "/u:test-user", "/from-stdin:force", "/scale-desktop:200"}
			if tc.desktop {
				wantArgs = append(wantArgs, "/f", "+dynamic-resolution")
				if strings.Contains(args, "/app:") || strings.Contains(args, "cmd:daemon") {
					t.Fatal("desktop received RemoteApp arguments", args)
				}
			} else {
				wantArgs = append(wantArgs, "cmd:daemon --bind "+a.Endpoint)
			}
			for _, want := range wantArgs {
				if !strings.Contains(args, want) {
					t.Errorf("missing %q in %q", want, args)
				}
			}
			if capture.Input != "test-only-password\n" || strings.Contains(args, "test-only-password") {
				t.Fatal(capture.Input, args)
			}
			for _, entry := range capture.Env {
				key, value, _ := strings.Cut(entry, "=")
				if strings.EqualFold(key, "http_proxy") {
					t.Fatal("proxy leaked", entry)
				}
				if key == "SDL_VIDEODRIVER" && value != "wayland" {
					t.Fatal(entry)
				}
			}
			stopCtx := root
			if ignoreTerm {
				var cancelStop context.CancelFunc
				stopCtx, cancelStop = context.WithTimeout(root, 20*time.Millisecond)
				defer cancelStop()
			}
			err := d.Stop(stopCtx)
			if ignoreTerm {
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			select {
			case event := <-events:
				if event.OperationID != 42 {
					t.Fatal(event)
				}
			case <-root.Done():
				t.Fatal(root.Err())
			}

		})
	}
}
func TestFreeRDPStartupCancellationCleansUpChild(t *testing.T) {
	t.Setenv("RDP_TEST_CHILD", "1")
	probing := make(chan struct{})
	a := fakeAgent(t, func(conn net.Conn) {
		var req agentRequest
		_ = json.NewDecoder(conn).Decode(&req)
		close(probing)
		_, _ = io.Copy(io.Discard, conn)
	})
	root, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	d := NewDisplay(root, displayTestConfig(), a, func(context.Context, controller.Event) error { return nil })
	ctx, cancelOperation := context.WithCancel(root)
	defer cancelOperation()
	done := make(chan error, 1)
	go func() { done <- d.Start(ctx, 1, false) }()
	select {
	case <-probing:
	case <-root.Done():
		t.Fatal(root.Err())
	}
	cancelOperation()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("startup succeeded")
		}
	case <-root.Done():
		t.Fatal("startup did not cancel")
	}
	select {
	case <-d.exited:
	default:
		t.Fatal("child not reaped")
	}
}
func TestRemoteAppPassesTokenFilePathWithoutSecret(t *testing.T) {
	c := displayTestConfig()
	c.AgentTokenFile = `C:\ProgramData\rdp-launcher\agent-token`
	args, err := c.args("192.0.2.10:49152", false)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, `cmd:daemon --bind 192.0.2.10:49152 --auth-token-file C:\ProgramData\rdp-launcher\agent-token`) {
		t.Fatal(joined)
	}
	if strings.Contains(joined, "test-only-password") {
		t.Fatal("token secret leaked into argv")
	}

	for _, path := range []string{
		`C:\Program Files\rdp-launcher\agent-token`,
		"C:\\token\tfile", "C:\\token\rfile", "C:\\token\nfile",
		"C:\\token\u00a0file", "C:\\token\u2003file",
		"invalid,path", `invalid"path`,
	} {
		c.AgentTokenFile = path
		if _, err = c.args("192.0.2.10:49152", false); err == nil {
			t.Fatalf("accepted ambiguous token file path %q", path)
		}
	}

	c.AgentTokenFile = `C:\ProgramData\rdp-launcher\agent-token`
	args, err = c.args("192.0.2.10:49152", true)
	if err != nil || strings.Contains(strings.Join(args, " "), "--auth-token-file") {
		t.Fatal("desktop mode received agent auth arguments", args, err)
	}
}

func TestFreeRDPIPv6AndProgramValidation(t *testing.T) {
	c := displayTestConfig()
	c.Host = "2001:db8::10"
	args, err := c.args("[2001:db8::10]:49152", false)
	if err != nil || args[0] != "/v:[2001:db8::10]" {
		t.Fatal(args, err)
	}
	c.Program = "invalid,program"
	if _, err = c.args("127.0.0.1:49152", false); err == nil {
		t.Fatal("accepted ambiguous app option")
	}
}
func TestDispatchChoosesEndpointBeforeSending(t *testing.T) {
	var background, interactive atomic.Int32
	back := jsonAgent(t, func(req agentRequest) any { background.Add(1); return map[string]any{"status": "ok"} })
	front := jsonAgent(t, func(req agentRequest) any { interactive.Add(1); return map[string]any{"status": "ok"} })
	s := &Session{agent: back, display: &Display{agent: front}}
	for _, need := range []controller.Requirement{controller.Interactive, controller.BackendOnly} {
		out := s.execute(context.Background(), controller.Operation{Kind: controller.Dispatch, Request: controller.Request{Action: "run", Target: "notepad", Need: need}})
		if out.Err != nil {
			t.Fatal(out.Err)
		}
	}
	out := s.execute(context.Background(), controller.Operation{Kind: controller.Dispatch, Request: controller.Request{Action: "desktop", Need: controller.Desktop}})
	if out.Err != nil || out.Data.(AgentResponse).Message != "display process started" {
		t.Fatal(out)
	}
	if background.Load() != 1 || interactive.Load() != 1 {
		t.Fatal(fmt.Sprint(background.Load(), interactive.Load()))
	}
}
