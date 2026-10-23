package adapter

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/controller"
)

// RDP_LIVE_TOKEN_FILE points to a private local copy of the temporary Windows
// test token. The value never enters test argv or logs.
func liveAgent(endpoint string) Agent {
	agent := Agent{Endpoint: endpoint}
	if path := os.Getenv("RDP_LIVE_TOKEN_FILE"); path != "" {
		agent.AuthToken = func(context.Context) (string, error) {
			data, err := os.ReadFile(path)
			if err != nil {
				return "", err
			}
			// Match Windows Token::load: one final LF/CRLF is a line ending,
			// not part of the token. Preserve spaces and reject all other CR/LF.
			if bytes.HasSuffix(data, []byte{'\n'}) {
				data = bytes.TrimSuffix(data, []byte{'\n'})
				data = bytes.TrimSuffix(data, []byte{'\r'})
			}
			if len(data) == 0 || bytes.ContainsAny(data, "\r\n") || !utf8.Valid(data) {
				return "", fmt.Errorf("invalid live token file: empty, multiline or non-UTF-8")
			}
			return string(data), nil
		}
	}
	return agent
}

// Opt-in: SSH stays unchanged for 32 seconds, then changes on the same
// subscription. Memory changes may still arrive; they are not heartbeats.
func TestLiveBackgroundWatchQuiet(t *testing.T) {
	endpoint := os.Getenv("RDP_LIVE_BACKGROUND")
	if endpoint == "" {
		t.Skip("set RDP_LIVE_BACKGROUND for the Windows long-connection test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	events := make(chan controller.BackendObserved, 8)
	stopped := make(chan error, 1)
	go func() {
		var previous controller.BackendObserved
		stopped <- liveAgent(endpoint).Watch(ctx, func(_ context.Context, e controller.Event) error {
			if state, ok := e.(controller.BackendObserved); ok && state != previous {
				previous = state
				events <- state
			}
			return nil
		})
	}()
	defer func() {
		cancel()
		select {
		case <-stopped:
		case <-time.After(time.Second):
			t.Error("watch did not stop")
		}
	}()
	for {
		select {
		case e := <-events:
			if e.Ready && e.SSH == controller.SSHZero {
				goto quiet
			}
		case err := <-stopped:
			t.Fatal(err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
quiet:
	select {
	case e := <-events:
		t.Fatalf("unexpected change during quiet test: %+v", e)
	case err := <-stopped:
		t.Fatal(err)
	case <-time.After(32 * time.Second):
	}
	command := exec.CommandContext(ctx, "ssh", "win11", "Start-Sleep -Seconds 2")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer command.Wait()
	select {
	case e := <-events:
		if e.SSH != controller.SSHActive {
			t.Fatal(e)
		}
	case err := <-stopped:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	t.Log("SSH unchanged for 32 seconds, then received SSH-active on the same connection; memory changes permitted")
}

// Opt-in integration test. Both the VM and new background endpoint must already
// be running. Idle policies stay disabled; this test never launches FreeRDP.
func TestLiveBackgroundOnlySession(t *testing.T) {
	endpoint := os.Getenv("RDP_LIVE_BACKGROUND")
	if endpoint == "" {
		t.Skip("set RDP_LIVE_BACKGROUND to an already running test backend")
	}
	socket := os.Getenv("RDP_LIVE_LIBVIRT_SOCKET")
	if socket == "" {
		socket = "/run/libvirt/libvirt-sock"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	session, err := Open(ctx, Config{LibvirtSocket: socket, LibvirtURI: "qemu:///system", Domain: "win11", AgentEndpoint: endpoint, AgentToken: liveAgent(endpoint).AuthToken})
	if err != nil {
		t.Fatal(err)
	}
	stopped := make(chan error, 1)
	go func() { stopped <- session.Run() }()
	defer func() {
		session.Close()
		select {
		case <-stopped:
		case <-time.After(time.Second):
			t.Error("session did not stop")
		}
	}()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		state, err := session.Controller.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if state.VM == controller.VMOff {
			t.Fatal("live test requires an already running VM")
		}
		if state.VM == controller.VMRunning && state.BackendReady {
			break
		}
		select {
		case err := <-stopped:
			t.Fatalf("session ended: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
	result := session.Controller.Do(ctx, controller.Request{Action: "status"})
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	response, ok := result.Data.(AgentResponse)
	if !ok || response.Role != "background" {
		t.Fatalf("unexpected response: %+v", result.Data)
	}
	state, err := session.Controller.Snapshot(ctx)
	if err != nil || state.Display {
		t.Fatal(state, err)
	}
	t.Logf("VM=%s backend=%s SSH=%s FreeRDP=%v", state.VM, response.Role, state.SSH, state.Display)
	// Disconnect only this test's real libvirt observation socket. The Session
	// must survive, subscribe again and refresh without another client request.
	session.vm.mu.Lock()
	oldSource := session.vm.current
	session.vm.mu.Unlock()
	if oldSource == nil {
		t.Fatal("missing acknowledged lifecycle source")
	}
	oldSource.shutdown()
	for {
		session.vm.mu.Lock()
		replaced := session.vm.current != nil && session.vm.current != oldSource
		session.vm.mu.Unlock()
		state, err := session.Controller.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if replaced && state.VM == controller.VMRunning && state.BackendReady {
			t.Log("real lifecycle disconnect recovered with a new subscription and VM snapshot; no new request or FreeRDP")
			break
		}
		select {
		case err := <-stopped:
			t.Fatal("session exited on source loss", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
}
