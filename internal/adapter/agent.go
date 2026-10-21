package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/controller"
)

type App struct {
	ShortcutPath string `json:"shortcut_path,omitempty" toml:"shortcut_path"`
	ID           string `json:"id"`
	Name         string `json:"name"`
	Type         string `json:"app_type" toml:"app_type"`
	Target       string `json:"target"`
	Arguments    string `json:"arguments,omitempty"`
	WorkingDir   string `json:"working_dir,omitempty" toml:"working_dir"`
	IconBase64   string `json:"icon_base64,omitempty" toml:"icon_base64"`
	Icon         string `json:"icon,omitempty"`
}
type AgentResponse struct {
	SessionID     *uint32                     `json:"session_id,omitempty"`
	SessionActive *bool                       `json:"session_active,omitempty"`
	Activity      *controller.SessionActivity `json:"activity,omitempty"`
	Status        string                      `json:"status"`
	Message       string                      `json:"message,omitempty"`
	Apps          []App                       `json:"apps,omitempty"`
	SSHActive     *bool                       `json:"ssh_active,omitempty"`
	Role          string                      `json:"role,omitempty"`
	Memory        *controller.GuestMemory     `json:"memory,omitempty"`
	Drives        [][2]string                 `json:"drives,omitempty"`
}
type agentRequest struct {
	Token        string `json:"token,omitempty"`
	User         string `json:"user,omitempty"`
	SessionID    uint32 `json:"session_id,omitempty"`
	Fingerprint  string `json:"fingerprint,omitempty"`
	RequireEmpty bool   `json:"require_empty"`
	Action       string `json:"action"`
	Target       string `json:"target,omitempty"`
	Params       string `json:"params,omitempty"`
	WorkingDir   string `json:"working_dir,omitempty"`
	File         string `json:"file,omitempty"`
	WithIcons    bool   `json:"with_icons,omitempty"`
}
type agentWatchFrame struct {
	Status    string          `json:"status"`
	SSHActive *bool           `json:"ssh_active"`
	Memory    json.RawMessage `json:"memory"`
	Message   string          `json:"message"`
}

// Agent talks to one Windows TCP endpoint. AuthToken is optional for legacy
// tests and explicit loopback-only deployments; when set, a missing or empty
// token fails locally instead of sending an unauthenticated request.
type Agent struct {
	Endpoint  string
	AuthToken func(context.Context) (string, error)
}

func (a Agent) token(ctx context.Context) (string, error) {
	if a.AuthToken == nil {
		return "", nil
	}
	token, err := a.AuthToken(ctx)
	if err != nil {
		return "", fmt.Errorf("agent auth token: %w", err)
	}
	if token == "" {
		return "", fmt.Errorf("agent auth token is empty")
	}
	return token, nil
}

func (a Agent) Call(ctx context.Context, req controller.Request) (AgentResponse, error) {
	return a.call(ctx, agentRequest{Action: req.Action, Target: req.Target, Params: req.Params, WorkingDir: req.WorkingDir, File: req.File, WithIcons: req.WithIcons})
}

func (a Agent) call(ctx context.Context, wire agentRequest) (AgentResponse, error) {
	var response AgentResponse
	timeout := 5 * time.Second
	if wire.Action == "list_apps" {
		timeout = time.Minute
	}
	if wire.Action == "inspect_session" || wire.Action == "logoff_session" {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	token, err := a.token(ctx)
	if err != nil {
		return response, err
	}
	wire.Token = token
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", a.Endpoint)
	if err != nil {
		return response, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if err = json.NewEncoder(conn).Encode(wire); err != nil {
		return response, err
	}
	// Existing list_apps responses include base64 icons. Keep the Rust client's
	// 32 MiB response boundary; JSON framing and decoding use the standard library.
	if err = json.NewDecoder(io.LimitReader(conn, 32*1024*1024)).Decode(&response); err != nil {
		return response, err
	}
	if response.Status != "ok" {
		return response, fmt.Errorf("agent: %s", response.Message)
	}
	return response, nil
}
func sshState(active *bool) controller.SSHState {
	switch {
	case active == nil:
		return controller.SSHUnknown
	case *active:
		return controller.SSHActive
	default:
		return controller.SSHZero
	}
}
func watchFailed(ctx context.Context, emit Sink, cause error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := emit(ctx, controller.BackendObserved{Ready: false, SSH: controller.SSHUnknown}); err != nil {
		return err
	}
	return fmt.Errorf("agent watch: %w", cause)
}

// A newly started VM may be Running before Windows starts the backend.
// Only initial connection establishment waits; a broken established stream
// still returns its error instead of replaying requests or hiding lost state.
func (a Agent) waitBackend(ctx context.Context) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", a.Endpoint)
		if err == nil {
			return conn, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for backend %s: %w", a.Endpoint, ctx.Err())
		case <-ticker.C:
		}
	}
}

// Watch subscribes to the resident Windows backend. A snapshot arrives first,
// then only state changes. Quiet connections have no application read timeout.
// The session explicitly resubscribes after VM state changes.
func (a Agent) Watch(ctx context.Context, emit Sink) error {
	token, err := a.token(ctx)
	if err != nil {
		return err
	}
	conn, err := a.waitBackend(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if err = conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	if err = json.NewEncoder(conn).Encode(agentRequest{Action: "watch_status", Token: token}); err != nil {
		return err
	}
	decoder := json.NewDecoder(conn)
	for {
		var frame agentWatchFrame
		if err = decoder.Decode(&frame); err != nil {
			return watchFailed(ctx, emit, err)
		}
		if frame.Status != "ok" {
			message := frame.Message
			if message == "" {
				message = "watch status " + frame.Status
			}
			return watchFailed(ctx, emit, errors.New(message))
		}
		if len(frame.Memory) > 0 {
			var memory *controller.GuestMemory
			if err = json.Unmarshal(frame.Memory, &memory); err != nil {
				return watchFailed(ctx, emit, err)
			}
			if err = emit(ctx, controller.MemoryObserved{Memory: memory}); err != nil {
				return err
			}
		}
		if err = emit(ctx, controller.BackendObserved{Ready: true, SSH: sshState(frame.SSHActive)}); err != nil {
			return err
		}
	}
}
