package host

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"
)

// The persistent lock inode serializes recovery and listening. Never unlink the
// lock file: a second opener could otherwise lock a different inode. The kernel
// releases the lock on process death, unlike the Unix socket pathname.
func listenController(socket string) (net.Listener, func(), error) {
	lock, err := os.OpenFile(socket+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, nil, err
	}
	release := func() { _ = lock.Close() }
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		release()
		return nil, nil, fmt.Errorf("another controller holds %s: %w", socket, err)
	}
	if info, statErr := os.Lstat(socket); statErr == nil {
		if info.Mode()&os.ModeSocket == 0 {
			release()
			return nil, nil, fmt.Errorf("refusing to replace non-socket %s", socket)
		}
		conn, dialErr := net.DialTimeout("unix", socket, 200*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			release()
			return nil, nil, fmt.Errorf("another controller is listening on %s", socket)
		}
		// A timeout, permission failure, or any other uncertain state is not proof
		// of staleness. In particular, never remove a live but busy listener.
		if !errors.Is(dialErr, syscall.ECONNREFUSED) {
			release()
			return nil, nil, fmt.Errorf("cannot establish whether %s is stale: %w", socket, dialErr)
		}
		current, err := os.Lstat(socket)
		if err != nil || !os.SameFile(info, current) {
			release()
			return nil, nil, fmt.Errorf("socket changed during recovery: %s", socket)
		}
		if err = os.Remove(socket); err != nil {
			release()
			return nil, nil, err
		}
	} else if !os.IsNotExist(statErr) {
		release()
		return nil, nil, statErr
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		release()
		return nil, nil, err
	}
	return listener, func() { _ = listener.Close(); release() }, nil
}
