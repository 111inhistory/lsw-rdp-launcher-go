package host

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestControllerSocketRecoversUncleanExit(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "controller.sock")
	old, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	old.SetUnlinkOnClose(false)
	_ = old.Close()
	listener, closeListener, err := listenController(socket)
	if err != nil {
		t.Fatal(err)
	}
	defer closeListener()
	conn, err := net.Dial("unix", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
}
func TestControllerSocketDoesNotRemoveLiveOrUnrelatedPath(t *testing.T) {
	for _, live := range []bool{false, true} {
		root := t.TempDir()
		socket := filepath.Join(root, "controller.sock")
		if live {
			_, closeListener, err := listenController(socket)
			if err != nil {
				t.Fatal(err)
			}
			defer closeListener()
		} else if err := os.WriteFile(socket, []byte("unrelated file"), 0600); err != nil {
			t.Fatal(err)
		}
		before, err := os.Lstat(socket)
		if err != nil {
			t.Fatal(err)
		}
		if listener, closeListener, err := listenController(socket); err == nil {
			closeListener()
			t.Fatal("replaced existing path", listener)
		}
		after, err := os.Lstat(socket)
		if err != nil || !os.SameFile(before, after) {
			t.Fatal("existing path changed", err)
		}
	}
}
func TestControllerSocketConcurrentStaleRecoveryHasOneReachableOwner(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "controller.sock")
	old, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	old.SetUnlinkOnClose(false)
	_ = old.Close()
	type result struct {
		listener net.Listener
		close    func()
		err      error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			listener, close, err := listenController(socket)
			results <- result{listener, close, err}
		}()
	}
	close(start)
	owners := 0
	for i := 0; i < 2; i++ {
		r := <-results
		if r.err == nil {
			owners++
			defer r.close()
		}
	}
	if owners != 1 {
		t.Fatalf("want one owner, got %d", owners)
	}
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal("winner became an orphan", err)
	}
	_ = conn.Close()
}

func TestControllerSocketSerializesRecoveryAndRestart(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "controller.sock")
	_, closeFirst, err := listenController(socket)
	if err != nil {
		t.Fatal(err)
	}
	if _, closeSecond, err := listenController(socket); err == nil {
		closeSecond()
		t.Fatal("second controller acquired live lock")
	}
	closeFirst()
	_, closeThird, err := listenController(socket)
	if err != nil {
		t.Fatal(err)
	}
	defer closeThird()
}
