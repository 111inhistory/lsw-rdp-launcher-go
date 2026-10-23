package main

import (
	"encoding/json"
	"net"
	"testing"

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/host"
)

// hostGreeting performs the version-2 client half of the private IPC
// handshake for tests that stand in for the resident controller.
func hostGreeting(t *testing.T, decoder *json.Decoder, encoder *json.Encoder) bool {
	t.Helper()
	var hello host.Request
	if err := decoder.Decode(&hello); err != nil {
		return false
	}
	if hello.Action != "hello" || hello.Version != host.ProtocolVersion {
		t.Errorf("bad controller handshake: %+v", hello)
		return false
	}
	return encoder.Encode(host.Response{Protocol: host.ProtocolVersion}) == nil
}

// serveHostOnce accepts one connection, completes the handshake, records the
// request, and either runs respond or sends a successful result.
func serveHostOnce(t *testing.T, listener net.Listener, respond func(*json.Decoder, *json.Encoder, host.Request)) <-chan host.Request {
	t.Helper()
	received := make(chan host.Request, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		decoder, encoder := json.NewDecoder(conn), json.NewEncoder(conn)
		if !hostGreeting(t, decoder, encoder) {
			return
		}
		var request host.Request
		if decoder.Decode(&request) != nil {
			return
		}
		received <- request
		if respond != nil {
			respond(decoder, encoder, request)
			return
		}
		_ = encoder.Encode(host.Response{Type: "result", Data: json.RawMessage(`{"status":"ok"}`)})
	}()
	return received
}
