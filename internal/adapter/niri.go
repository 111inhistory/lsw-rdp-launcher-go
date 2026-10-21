package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"net"

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/controller"
)

const DefaultFreeRDPAppID = "com.freerdp.client.sdl3"

type niriWindow struct {
	ID    uint64 `json:"id"`
	AppID string `json:"app_id"`
}
type niriEvent struct {
	Snapshot *struct {
		Windows []niriWindow `json:"windows"`
	} `json:"WindowsChanged"`
	Changed *struct {
		Window niriWindow `json:"window"`
	} `json:"WindowOpenedOrChanged"`
	Closed *struct {
		ID uint64 `json:"id"`
	} `json:"WindowClosed"`
}

// WatchNiri uses the documented Unix-socket JSON protocol. Only the three
// window events needed by the controller are represented here.
func WatchNiri(ctx context.Context, socket, appID string, emit Sink) error {
	if appID == "" {
		appID = DefaultFreeRDPAppID
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		_ = emit(ctx, controller.WindowsObserved{Count: -1})
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	defer func() { _ = emit(ctx, controller.WindowsObserved{Count: -1}) }()
	if err = json.NewEncoder(conn).Encode("EventStream"); err != nil {
		return err
	}
	decoder := json.NewDecoder(conn)
	var reply struct {
		Ok  string
		Err string
	}
	if err = decoder.Decode(&reply); err != nil {
		return err
	}
	if reply.Ok != "Handled" {
		return fmt.Errorf("Niri subscription rejected: %s", reply.Err)
	}
	windows := map[uint64]bool{}
	count := -1
	for {
		var event niriEvent
		if err = decoder.Decode(&event); err != nil {
			return err
		}
		switch {
		case event.Snapshot != nil:
			clear(windows)
			for _, w := range event.Snapshot.Windows {
				windows[w.ID] = w.AppID == appID
			}
		case count < 0:
			continue // Wait for a full snapshot.
		case event.Changed != nil:
			w := event.Changed.Window
			windows[w.ID] = w.AppID == appID
		case event.Closed != nil:
			delete(windows, event.Closed.ID)
		default:
			continue
		}
		next := 0
		for _, rdp := range windows {
			if rdp {
				next++
			}
		}
		if next != count {
			count = next
			if err = emit(ctx, controller.WindowsObserved{Count: count}); err != nil {
				return err
			}
		}
	}
}
