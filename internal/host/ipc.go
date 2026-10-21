package host

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/adapter"
	"github.com/111inhistory/lsw-rdp-launcher-go/internal/controller"
	"golang.org/x/sync/errgroup"
)

type Request struct {
	Action  string             `json:"action"`
	Request controller.Request `json:"request,omitempty"`
}
type Response struct {
	Data  json.RawMessage `json:"data,omitempty"`
	Error string          `json:"error,omitempty"`
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
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return
	}
	var request Request
	if err = json.Unmarshal(line, &request); err != nil {
		_ = json.NewEncoder(conn).Encode(Response{Error: err.Error()})
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { var extra [1]byte; _, _ = conn.Read(extra[:]); cancel() }()
	var data any
	switch request.Action {
	case "status":
		var snapshot controller.Snapshot
		snapshot, err = c.Snapshot(ctx)
		data = status(snapshot)
	case "lock", "unlock":
		err = c.SetLocked(ctx, request.Action == "lock")
	case "switch-display":
		// A distinct action makes old services reject checked switching instead
		// of silently ignoring a new flag and disconnecting without inspection.
		if request.Request.Need != controller.Interactive && request.Request.Need != controller.Desktop {
			err = fmt.Errorf("switch requires remoteapp or desktop")
			break
		}
		request.Request.Action = "connect"
		request.Request.SwitchDisplay = true
		result := c.Do(ctx, request.Request)
		data, err = result.Data, result.Err
	case "request":
		result := c.Do(ctx, request.Request)
		data, err = result.Data, result.Err
	case "shutdown":
	default:
		err = fmt.Errorf("unknown host action %q", request.Action)
	}
	var response Response
	if err != nil {
		response.Error = err.Error()
	} else if data != nil {
		response.Data, err = json.Marshal(data)
		if err != nil {
			response.Error = err.Error()
		}
	}
	_ = json.NewEncoder(conn).Encode(response)
	if request.Action == "shutdown" {
		shutdown()
	}
}
func Call(ctx context.Context, socket string, request Request) (json.RawMessage, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, fmt.Errorf("connect controller (start serve first): %w", err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if err = json.NewEncoder(conn).Encode(request); err != nil {
		return nil, err
	}
	var response Response
	if err = json.NewDecoder(conn).Decode(&response); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	if response.Error != "" {
		return response.Data, errors.New(response.Error)
	}
	return response.Data, nil
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
