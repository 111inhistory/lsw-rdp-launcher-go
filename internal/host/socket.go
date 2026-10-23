package host

import (
	"fmt"
	"net"
	"os"
	"syscall"
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
