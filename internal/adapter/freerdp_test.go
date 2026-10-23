package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/signal"
	"strings"
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
	return DisplayConfig{
		Binary:            os.Args[0],
		Host:              "192.0.2.10",
		User:              "test-user",
		Program:           `C:\Program Files\RemoteApp\agent.exe`,
		ClientHostname:    "rdp-test-client",
		VideoDriver:       "wayland",
		ScaleDesktop:      200,
		Clipboard:         true,
		IgnoreCertificate: true,
		Password:          func(context.Context) (string, error) { return "test-only-password", nil },
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
			config.Env = map[string]string{"RDP_ENV_TEST": "literal $HOME = value", "RDP_ENV_EMPTY": ""}
			d := NewDisplay(root, config, func(_ context.Context, event controller.Event) error {
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
			wantArgs := []string{
				"/v:192.0.2.10", "/u:test-user", "/from-stdin:force",
				"/client-hostname:rdp-test-client", "+clipboard", "/cert:ignore", "/scale-desktop:200",
			}
			if tc.desktop {
				wantArgs = append(wantArgs, "/f", "+dynamic-resolution")
				if strings.Contains(args, "/app:") || strings.Contains(args, "remoteapp-shell") {
					t.Fatal("desktop received RemoteApp arguments", args)
				}
			} else {
				wantArgs = append(wantArgs, `program:C:\Program Files\RemoteApp\agent.exe,cmd:remoteapp-shell`)
				for _, forbidden := range []string{"--bind", "--auth-token-file", "49152", "daemon"} {
					if strings.Contains(args, forbidden) {
						t.Fatalf("RemoteApp used obsolete interactive entry %q in %q", forbidden, args)
					}
				}
			}
			for _, want := range wantArgs {
				if !strings.Contains(args, want) {
					t.Errorf("missing %q in %q", want, args)
				}
			}
			if capture.Input != "test-only-password\n" || strings.Contains(args, "test-only-password") {
				t.Fatal(capture.Input, args)
			}
			for key, value := range config.Env {
				found := false
				for _, entry := range capture.Env {
					if entry == key+"="+value {
						found = true
					}
				}
				if !found {
					t.Fatal("child did not receive configured environment", key)
				}
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

func TestFreeRDPStartFailsBeforeSpawnWithoutPassword(t *testing.T) {
	config := displayTestConfig()
	config.Password = nil
	d := NewDisplay(context.Background(), config, nil)
	if err := d.Start(context.Background(), 1, false); err == nil || d.command != nil {
		t.Fatal("missing password provider reached process startup", err)
	}
}

func TestFreeRDPStartWithCanceledRootDoesNotLeaveChild(t *testing.T) {
	root, cancel := context.WithCancel(context.Background())
	cancel()
	d := NewDisplay(root, displayTestConfig(), nil)
	if err := d.Start(context.Background(), 1, false); err == nil {
		t.Fatal("startup succeeded with canceled owned root")
	}
	if d.command != nil {
		t.Fatal("canceled startup kept a process")
	}
}

func TestRemoteAppUsesShellKeepaliveWithoutHostEndpoint(t *testing.T) {
	c := displayTestConfig()
	args, err := c.args(false)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, `cmd:remoteapp-shell`) {
		t.Fatal(joined)
	}
	if !strings.Contains(joined, `/client-hostname:rdp-test-client`) {
		t.Fatal(joined)
	}
	for _, forbidden := range []string{"--bind", "--auth-token-file", "49152", "daemon", "test-only-password"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("RemoteApp args contain %q: %s", forbidden, joined)
		}
	}

	args, err = c.args(true)
	if err != nil {
		t.Fatal(err)
	}
	joined = strings.Join(args, " ")
	if strings.Contains(joined, "/app:") || strings.Contains(joined, "remoteapp-shell") {
		t.Fatal("desktop received RemoteApp arguments", joined)
	}
}

func TestFreeRDPClientHostnameIsStableAndValidated(t *testing.T) {
	c := displayTestConfig()
	c.ClientHostname = ""
	args, err := c.args(false)
	if err != nil || !strings.Contains(strings.Join(args, " "), "/client-hostname:rdp-launcher") {
		t.Fatal(args, err)
	}

	c.ClientHostname = strings.Repeat("A", 15)
	if _, err = c.args(false); err != nil {
		t.Fatal("15-character hostname rejected", err)
	}
	if _, err = c.args(false); err != nil {
		t.Fatal("hostname is not stable across argument construction", err)
	}
	for _, invalid := range []string{
		strings.Repeat("A", 16), "with space", "tab\tname", "café", "line\nbreak",
	} {
		c.ClientHostname = invalid
		if _, err = c.args(false); err == nil {
			t.Fatalf("accepted invalid client hostname %q", invalid)
		}
	}

	c.ClientHostname = "stable-id"
	c.ExtraArgs = []string{"/client-hostname:override"}
	if _, err = c.args(false); err == nil {
		t.Fatal("accepted /client-hostname override in extra args")
	}
	c.ExtraArgs = []string{"/network:auto"}
	args, err = c.args(false)
	if err != nil || !strings.Contains(strings.Join(args, " "), "/network:auto") {
		t.Fatal(args, err)
	}
}

func TestFreeRDPIPv6AndProgramValidation(t *testing.T) {
	c := displayTestConfig()
	c.Host = "2001:db8::10"
	args, err := c.args(false)
	if err != nil || args[0] != "/v:[2001:db8::10]" {
		t.Fatal(args, err)
	}
	for _, invalid := range []string{"", "invalid,program", `invalid"program`, "line\nprogram"} {
		c.Program = invalid
		if _, err = c.args(false); err == nil {
			t.Fatalf("accepted ambiguous program %q", invalid)
		}
	}
	// Desktop mode does not use the RemoteApp program, so it stays valid.
	c.Program = ""
	args, err = c.args(true)
	if err != nil || strings.Contains(strings.Join(args, " "), "/app:") {
		t.Fatal(args, err)
	}
}
