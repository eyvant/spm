//go:build darwin

package supervisor

import (
	"fmt"
	"io"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// newOutputPipe creates the pipe a service writes its output to. Both ends
// are close-on-exec; the child receives the write end as its stdout and
// stderr, and the read end is passed explicitly across a supervisor re-exec.
func newOutputPipe() (int, *os.File, error) {
	var fds [2]int
	if err := unix.Pipe(fds[:]); err != nil {
		return -1, nil, fmt.Errorf("create output pipe: %w", err)
	}
	unix.CloseOnExec(fds[0])
	unix.CloseOnExec(fds[1])
	return fds[0], os.NewFile(uintptr(fds[1]), "service-output"), nil
}

// pumpOutput copies the service's output into dst until every writer has
// closed the pipe (it then returns true), halt closes, or drain closes and the
// pipe stays empty for drainQuiet. It reads only after poll reports data, so a
// read never blocks: when it returns false no output is in flight, and the
// pipe can be handed to the next supervisor image without losing a byte.
func pumpOutput(fd int, dst io.Writer, halt, drain <-chan struct{}) bool {
	buffer := make([]byte, 64*1024)
	quiet := newQuietTimer(drain)
	for {
		select {
		case <-halt:
			return false
		default:
		}
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		ready, err := unix.Poll(fds, int(pumpInterval/time.Millisecond))
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return true
		}
		if ready == 0 {
			if quiet.done() {
				return false
			}
			continue
		}
		count, err := unix.Read(fd, buffer)
		if count > 0 {
			_, _ = dst.Write(buffer[:count])
			quiet.reset()
		}
		if err == unix.EINTR || err == unix.EAGAIN {
			continue
		}
		if count <= 0 || err != nil {
			return true
		}
	}
}
