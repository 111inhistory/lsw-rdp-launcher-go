package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"testing"

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/controller"
)

func TestAgentMemoryNotificationAndNull(t *testing.T) {
	a := watchAgent(t, func(conn net.Conn) {
		encoder := json.NewEncoder(conn)
		for _, memory := range []any{map[string]any{"total_bytes": 4000, "available_bytes": 200, "low": true}, nil} {
			if err := encoder.Encode(map[string]any{"protocol": agentProtocol, "status": "ok", "kind": "changed", "ssh_active": false, "memory": memory}); err != nil {
				t.Error(err)
				return
			}
		}
		_, _ = io.Copy(io.Discard, conn)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var seen []*controller.GuestMemory
	backend := 0
	err := a.Watch(ctx, func(_ context.Context, event controller.Event) error {
		switch e := event.(type) {
		case controller.MemoryObserved:
			seen = append(seen, e.Memory)
		case controller.BackendObserved:
			backend++
			if backend == 2 {
				cancel()
			}
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) || len(seen) != 2 || seen[0] == nil || seen[0].TotalBytes != 4000 || seen[0].AvailableBytes != 200 || seen[1] != nil || backend != 2 {
		t.Fatal(seen, backend, err)
	}
}
func TestAgentStatusIncludesMemory(t *testing.T) {
	a := jsonAgent(t, func(agentRequest) any {
		return map[string]any{"protocol": agentProtocol, "status": "ok", "role": "background", "memory": map[string]any{"total_bytes": 4000, "available_bytes": 2000, "low": false}}
	})
	out, err := a.Call(context.Background(), controller.Request{Action: "status"})
	if err != nil || out.Memory == nil || out.Memory.TotalBytes != 4000 || out.Memory.AvailableBytes != 2000 {
		t.Fatal(out, err)
	}
}
func TestAgentMalformedMemoryInvalidatesBackend(t *testing.T) {
	a := watchAgent(t, func(conn net.Conn) {
		_ = json.NewEncoder(conn).Encode(map[string]any{"protocol": agentProtocol, "status": "ok", "memory": map[string]any{"total_bytes": "bad"}})
	})
	var seen []controller.BackendObserved
	err := a.Watch(context.Background(), func(_ context.Context, event controller.Event) error {
		if e, ok := event.(controller.BackendObserved); ok {
			seen = append(seen, e)
		}
		return nil
	})
	if err == nil || len(seen) != 1 || seen[0].Ready || seen[0].SSH != controller.SSHUnknown {
		t.Fatal(seen, err)
	}
}
