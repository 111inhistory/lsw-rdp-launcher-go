package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/adapter"
	"github.com/111inhistory/lsw-rdp-launcher-go/internal/controller"
	"golang.org/x/sync/errgroup"
)

// ProtocolVersion is the private host IPC revision. A CLI and the resident
// service must come from the same build; there is no mixed-version fallback.
const ProtocolVersion = 2

type Request struct {
	Version int                `json:"version,omitempty"`
	Action  string             `json:"action"`
	Request controller.Request `json:"request,omitempty"`
}
type Response struct {
	// Protocol is set only by the handshake answer. Result frames carry the
	// business payload in Data/Error and confirmation frames carry Prompt.
	Protocol int                      `json:"protocol,omitempty"`
	Type     string                   `json:"type,omitempty"` // confirm or result
	ID       uint64                   `json:"id,omitempty"`
	Prompt   *controller.SwitchPrompt `json:"prompt,omitempty"`
	Data     json.RawMessage          `json:"data,omitempty"`
	Error    string                   `json:"error,omitempty"`
}

// decision is the only frame a CLI may send while a confirmation is pending.
// It carries no session-control authority: the controller already decided
// which actions are allowed and validates the reply again.
type decision struct {
	Type     string                    `json:"type"`
	ID       uint64                    `json:"id"`
	Decision controller.SwitchDecision `json:"decision"`
}

func protocolMismatch() string {
	return fmt.Sprintf("controller IPC protocol mismatch: this build requires version %d; upgrade the controller service and rdp-launcher together", ProtocolVersion)
}

func Run(ctx context.Context, c Config, socket string) error {
	config, err := c.Adapter()
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(socket), 0700); err != nil {
		return err
	}
	listener, closeListener, err := listenController(socket)
	if err != nil {
		return fmt.Errorf("listen %s: %w", socket, err)
	}
	defer closeListener()
	if err = os.Chmod(socket, 0600); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	group, ctx := errgroup.WithContext(ctx)
	session, err := adapter.Open(ctx, config)
	if err != nil {
		return err
	}
	group.Go(session.Run)
	group.Go(func() error { return Serve(ctx, listener, session.Controller, cancel) })
	err = group.Wait()
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// One request per connection. Disconnect cancels only that caller's queued work.
func Serve(ctx context.Context, listener net.Listener, c *controller.Controller, shutdown context.CancelFunc) error {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(ctx, func() { listener.Close() })
	var clients sync.WaitGroup
	defer func() { cancel(); stop(); listener.Close(); clients.Wait() }()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		clients.Add(1)
		go func() { defer clients.Done(); handle(ctx, conn, c, shutdown) }()
	}
}
func handle(ctx context.Context, conn net.Conn, c *controller.Controller, shutdown context.CancelFunc) {
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	decoder := json.NewDecoder(conn)
	encoder := json.NewEncoder(conn)
	var writeMu sync.Mutex
	write := func(response Response) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return encoder.Encode(response)
	}

	// The CLI greets first so a new CLI against an old service (which does not
	// know "hello") fails visibly instead of silently dropping new fields.
	var hello Request
	if err := decoder.Decode(&hello); err != nil {
		return
	}
	if hello.Action != "hello" || hello.Version != ProtocolVersion {
		_ = write(Response{Error: protocolMismatch()})
		return
	}
	if err := write(Response{Protocol: ProtocolVersion}); err != nil {
		return
	}

	var request Request
	if err := decoder.Decode(&request); err != nil {
		_ = write(Response{Type: "result", Error: err.Error()})
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type inbound struct {
		line []byte
		err  error
	}
	inboundCh := make(chan inbound, 4)
	go func() {
		for {
			var raw json.RawMessage
			err := decoder.Decode(&raw)
			event := inbound{line: raw, err: err}
			select {
			case inboundCh <- event:
			case <-ctx.Done():
				return
			}
			if err != nil {
				// A disconnect cancels only this connection's queued work.
				cancel()
				return
			}
		}
	}()

	var nextID uint64
	confirm := func(cctx context.Context, prompt controller.SwitchPrompt) (controller.SwitchDecision, error) {
		nextID++
		id := nextID
		if err := write(Response{Type: "confirm", ID: id, Prompt: &prompt}); err != nil {
			return "", err
		}
		for {
			select {
			case <-cctx.Done():
				return "", cctx.Err()
			case event := <-inboundCh:
				if event.err != nil {
					return "", fmt.Errorf("controller IPC disconnected while waiting for a choice: %w", event.err)
				}
				var reply decision
				if err := json.Unmarshal(event.line, &reply); err != nil {
					return "", fmt.Errorf("invalid choice response: %w", err)
				}
				if reply.Type != "decision" || reply.ID != id {
					return "", fmt.Errorf("choice response does not match the pending task")
				}
				if !prompt.Allows(reply.Decision) {
					return "", fmt.Errorf("choice %q is not allowed for this task", reply.Decision)
				}
				return reply.Decision, nil
			}
		}
	}

	var data any
	var err error
	switch request.Action {
	case "status":
		var snapshot controller.Snapshot
		snapshot, err = c.Snapshot(ctx)
		data = status(snapshot)
	case "lock", "unlock":
		err = c.SetLocked(ctx, request.Action == "lock")
	case "request":
		request.Request.Confirm = confirm
		result := c.Do(ctx, request.Request)
		data, err = result.Data, result.Err
	case "shutdown":
	default:
		err = fmt.Errorf("unknown host action %q", request.Action)
	}
	response := resultResponse(data, err)
	_ = write(response)
	if request.Action == "shutdown" {
		shutdown()
	}
}

func resultResponse(data any, businessErr error) Response {
	response := Response{Type: "result"}
	if data != nil {
		encoded, marshalErr := json.Marshal(data)
		if marshalErr != nil {
			if businessErr == nil {
				businessErr = marshalErr
			} else {
				businessErr = fmt.Errorf("%v; partial result could not be encoded: %w", businessErr, marshalErr)
			}
		} else {
			response.Data = encoded
		}
	}
	if businessErr != nil {
		response.Error = businessErr.Error()
	}
	return response
}

// CallWithConfirm performs one request. The callback runs on the calling
// goroutine, so an in-process GUI (MyGo) can own the main goroutine, and is
// invoked only for a confirmation frame correlated with this request.
func CallWithConfirm(ctx context.Context, socket string, request Request, confirm func(context.Context, controller.SwitchPrompt) (controller.SwitchDecision, error)) (json.RawMessage, error) {
	conn, decoder, encoder, stop, err := connect(ctx, socket)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	defer stop()
	if err = encoder.Encode(request); err != nil {
		return nil, err
	}
	for {
		var response Response
		if err = decoder.Decode(&response); err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, err
		}
		switch response.Type {
		case "confirm":
			if confirm == nil {
				return nil, fmt.Errorf("controller requested a choice, but this command cannot prompt")
			}
			if response.Prompt == nil {
				return nil, fmt.Errorf("controller sent an empty choice prompt")
			}
			choiceCtx, cancel := context.WithTimeout(ctx, ConfirmationTimeout)
			value, choiceErr := confirm(choiceCtx, *response.Prompt)
			cancel()
			if choiceErr != nil {
				// Release the queue head instead of waiting for the deadline.
				_ = encoder.Encode(decision{Type: "decision", ID: response.ID, Decision: controller.SwitchCancel})
				return nil, choiceErr
			}
			if err = encoder.Encode(decision{Type: "decision", ID: response.ID, Decision: value}); err != nil {
				return nil, err
			}
		case "", "result":
			if response.Error != "" {
				return response.Data, errors.New(response.Error)
			}
			return response.Data, nil
		default:
			return nil, fmt.Errorf("unexpected controller IPC frame %q", response.Type)
		}
	}
}

// ConfirmationTimeout bounds one interactive choice. The controller applies the
// same limit independently and also caps it with the caller's total deadline.
const ConfirmationTimeout = 60 * time.Second

func connect(ctx context.Context, socket string) (net.Conn, *json.Decoder, *json.Encoder, func() bool, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("connect controller (run rdp-launcher start first): %w", err)
	}
	// A service that accepts and then stays silent must not outlive the
	// caller's deadline. The same cancellation covers the handshake and the
	// request that follows it.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	fail := func(err error) (net.Conn, *json.Decoder, *json.Encoder, func() bool, error) {
		stop()
		_ = conn.Close()
		if ctx.Err() != nil {
			return nil, nil, nil, nil, ctx.Err()
		}
		return nil, nil, nil, nil, err
	}
	decoder := json.NewDecoder(conn)
	encoder := json.NewEncoder(conn)
	if err = encoder.Encode(Request{Version: ProtocolVersion, Action: "hello"}); err != nil {
		return fail(err)
	}
	var hello Response
	if err = decoder.Decode(&hello); err != nil {
		return fail(fmt.Errorf("controller IPC handshake: %w", err))
	}
	if hello.Protocol != ProtocolVersion {
		message := protocolMismatch()
		if hello.Error != "" {
			message += ": " + hello.Error
		}
		return fail(errors.New(message))
	}
	return conn, decoder, encoder, stop, nil
}

func Call(ctx context.Context, socket string, request Request) (json.RawMessage, error) {
	return CallWithConfirm(ctx, socket, request, nil)
}

func status(s controller.Snapshot) any {
	type memory struct {
		controller.MemoryState
		LastError string
	}
	return struct {
		controller.Snapshot
		LastError string
		Memory    memory
	}{s, errorText(s.LastError), memory{s.Memory, errorText(s.Memory.LastError)}}
}
func errorText(err error) string {
	if err != nil {
		return err.Error()
	}
	return ""
}
