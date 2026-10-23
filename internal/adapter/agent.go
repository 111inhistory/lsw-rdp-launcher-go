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

// The Windows agent and host private protocol is upgraded together; there is no
// silent fallback to the old global-interactive protocol.
const agentProtocol = 2

const protocolMismatch = "protocol mismatch: Windows agent and host must be upgraded together (agent protocol 2)"

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
	Protocol      int                         `json:"protocol"`
	Status        string                      `json:"status"`
	Message       string                      `json:"message,omitempty"`
	Role          string                      `json:"role,omitempty"`
	SessionRef    *controller.SessionRef      `json:"session_ref,omitempty"`
	Snapshot      *controller.SessionSnapshot `json:"snapshot,omitempty"`
	SessionActive *bool                       `json:"session_active,omitempty"`
	Activity      *controller.SessionActivity `json:"activity,omitempty"`
	Outcome       string                      `json:"outcome,omitempty"`
	Apps          []App                       `json:"apps,omitempty"`
	SSHActive     *bool                       `json:"ssh_active,omitempty"`
	Memory        *controller.GuestMemory     `json:"memory,omitempty"`
	Drives        [][2]string                 `json:"drives,omitempty"`
}
type agentRequest struct {
	Protocol   int                    `json:"protocol"`
	Token      string                 `json:"token,omitempty"`
	User       string                 `json:"user,omitempty"`
	SessionRef *controller.SessionRef `json:"session_ref,omitempty"`
	Action     string                 `json:"action"`
	Target     string                 `json:"target,omitempty"`
	Params     string                 `json:"params,omitempty"`
	WorkingDir string                 `json:"working_dir,omitempty"`
	File       string                 `json:"file,omitempty"`
	WithIcons  bool                   `json:"with_icons,omitempty"`
}
type agentWatchFrame struct {
	Protocol int    `json:"protocol"`
	Status   string `json:"status"`
	Kind     string `json:"kind"`
	// RawMessage distinguishes an omitted field from an explicit JSON null.
	SSHActive json.RawMessage             `json:"ssh_active"`
	Memory    json.RawMessage             `json:"memory"`
	Snapshot  *controller.SessionSnapshot `json:"snapshot"`
	Message   string                      `json:"message"`
	Outcome   string                      `json:"outcome"`
}

// Agent talks to the one host-facing Windows backend endpoint. AuthToken is
// optional for legacy loopback-only tests; when set, a missing or empty token
// fails locally instead of sending an unauthenticated request.
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

// Call sends a backend-only request. Actions that execute in a Windows logon
// must use CallInSession with the bound SessionRef.
func (a Agent) Call(ctx context.Context, req controller.Request) (AgentResponse, error) {
	return a.call(ctx, requestFromController(req))
}

// CallInSession routes a user action to the verified Windows logon identity.
func (a Agent) CallInSession(ctx context.Context, req controller.Request, ref controller.SessionRef) (AgentResponse, error) {
	wire := requestFromController(req)
	wire.SessionRef = &ref
	return a.call(ctx, wire)
}

// InspectSessions requests the complete snapshot of the configured account's
// logons. The backend ignores an empty user value and uses its configured SID.
func (a Agent) InspectSessions(ctx context.Context, user string) (AgentResponse, error) {
	return a.call(ctx, agentRequest{Action: "inspect_sessions", User: user})
}

// SessionStatus asks the on-demand user worker to prove it is running in the
// requested logon before the host dispatches business actions there.
func (a Agent) SessionStatus(ctx context.Context, ref controller.SessionRef) (AgentResponse, error) {
	return a.call(ctx, agentRequest{Action: "session_status", SessionRef: &ref})
}

func requestFromController(req controller.Request) agentRequest {
	return agentRequest{Action: req.Action, Target: req.Target, Params: req.Params, WorkingDir: req.WorkingDir, File: req.File, WithIcons: req.WithIcons}
}

func agentTimeout(action string) time.Duration {
	switch action {
	case "list_apps":
		return time.Minute
	case "inspect_sessions", "session_status":
		return 30 * time.Second
	case "logoff_session":
		// The agent's logoff confirmation shares a 30 second deadline; leave
		// room for the response frame after that deadline.
		return 35 * time.Second
	default:
		return 5 * time.Second
	}
}

func agentActionMayHaveSideEffect(action string) bool {
	switch action {
	case "run", "open", "logoff_session":
		return true
	default:
		return false
	}
}

// A transport failure after a mutating request may mean the action ran even
// though no response arrived. Preserve the underlying error for errors.Is, and
// never imply that the action failed or is safe to retry automatically.
func wrapSubmittedActionError(action string, err error) error {
	if err == nil || !agentActionMayHaveSideEffect(action) {
		return err
	}
	return fmt.Errorf("%s submitted; remote outcome unknown; no automatic retry: %w", action, err)
}

func agentResponseError(response AgentResponse) error {
	message := response.Message
	if message == "" {
		message = "request failed"
	}
	if response.Outcome != "" {
		return fmt.Errorf("agent: %s (outcome %s)", message, response.Outcome)
	}
	return fmt.Errorf("agent: %s", message)
}

func (a Agent) call(ctx context.Context, wire agentRequest) (AgentResponse, error) {
	var response AgentResponse
	wire.Protocol = agentProtocol
	timeout := agentTimeout(wire.Action)
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
		return response, wrapSubmittedActionError(wire.Action, err)
	}
	// list_apps responses can carry base64 icons. Keep the Rust agent's 32 MiB
	// response boundary; JSON framing and decoding use the standard library.
	if err = json.NewDecoder(io.LimitReader(conn, 32*1024*1024)).Decode(&response); err != nil {
		return response, wrapSubmittedActionError(wire.Action, err)
	}
	if response.Protocol != agentProtocol {
		return response, errors.New(protocolMismatch)
	}
	if response.Snapshot != nil {
		response.Snapshot.ObservedAt = time.Now()
	}
	if response.Status != "ok" {
		return response, agentResponseError(response)
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

func invalidSnapshot(last *controller.SessionSnapshot) controller.SessionSnapshot {
	if last == nil {
		return controller.SessionSnapshot{Fresh: false}
	}
	snapshot := *last
	snapshot.Fresh = false
	return snapshot
}

func watchFailed(ctx context.Context, emit Sink, last *controller.SessionSnapshot, cause error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := emit(ctx, controller.BackendObserved{Ready: false, SSH: controller.SSHUnknown}); err != nil {
		return err
	}
	if err := emit(ctx, controller.SessionsObserved{Snapshot: invalidSnapshot(last)}); err != nil {
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
	if err = json.NewEncoder(conn).Encode(agentRequest{Protocol: agentProtocol, Action: "watch_status", Token: token}); err != nil {
		return err
	}
	decoder := json.NewDecoder(conn)
	var lastSnapshot *controller.SessionSnapshot
	ssh := controller.SSHUnknown
	for {
		var frame agentWatchFrame
		if err = decoder.Decode(&frame); err != nil {
			return watchFailed(ctx, emit, lastSnapshot, err)
		}
		if frame.Protocol != agentProtocol {
			return watchFailed(ctx, emit, lastSnapshot, errors.New(protocolMismatch))
		}
		if frame.Status != "ok" {
			message := frame.Message
			if message == "" {
				message = "watch status " + frame.Status
			}
			return watchFailed(ctx, emit, lastSnapshot, errors.New(message))
		}
		if len(frame.Memory) > 0 {
			var memory *controller.GuestMemory
			if err = json.Unmarshal(frame.Memory, &memory); err != nil {
				return watchFailed(ctx, emit, lastSnapshot, err)
			}
			if err = emit(ctx, controller.MemoryObserved{Memory: memory}); err != nil {
				return err
			}
		}
		if len(frame.SSHActive) > 0 {
			var active *bool
			if err = json.Unmarshal(frame.SSHActive, &active); err != nil {
				return watchFailed(ctx, emit, lastSnapshot, err)
			}
			ssh = sshState(active)
		}
		if frame.Snapshot != nil {
			snapshot := *frame.Snapshot
			snapshot.ObservedAt = time.Now()
			lastSnapshot = &snapshot
			if err = emit(ctx, controller.SessionsObserved{Snapshot: snapshot}); err != nil {
				return err
			}
		}
		if err = emit(ctx, controller.BackendObserved{Ready: true, SSH: ssh}); err != nil {
			return err
		}
	}
}
