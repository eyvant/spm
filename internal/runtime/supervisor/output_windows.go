//go:build windows

package supervisor

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
	"unsafe"

	"github.com/eyvant/spm/internal/runtime/common"
	"golang.org/x/sys/windows"
)

var procPeekNamedPipe = kernel32.NewProc("PeekNamedPipe")

// newOutputPipe creates the pipe a service writes its output to. The read
// end stays in the supervisor; the write end is handed to the child and then
// closed here.
func newOutputPipe() (windows.Handle, *os.File, error) {
	var read, write windows.Handle
	if err := windows.CreatePipe(&read, &write, nil, 64*1024); err != nil {
		return 0, nil, fmt.Errorf("create output pipe: %w", err)
	}
	return read, os.NewFile(uintptr(write), "service-output"), nil
}

// pumpOutput copies the service's output into dst until every writer has
// closed the pipe (it then returns true), halt closes, or drain closes and the
// pipe stays empty for drainQuiet. It reads only what PeekNamedPipe reports as
// available, so a read never blocks: when it returns false no output is in
// flight, and the pipe can be handed to another supervisor without losing a
// byte.
func pumpOutput(read windows.Handle, dst io.Writer, halt, drain <-chan struct{}) bool {
	buffer := make([]byte, 64*1024)
	quiet := newQuietTimer(drain)
	for {
		select {
		case <-halt:
			return false
		default:
		}
		var available uint32
		ok, _, _ := procPeekNamedPipe.Call(uintptr(read), 0, 0, 0, uintptr(unsafe.Pointer(&available)), 0)
		if ok == 0 {
			// ERROR_BROKEN_PIPE: the service and everything it started
			// have closed their end. PeekNamedPipe says so even while
			// output is still buffered; with no writer left ReadFile cannot
			// block, so read until it reports the end too.
			for {
				var count uint32
				if err := windows.ReadFile(read, buffer, &count, nil); err != nil || count == 0 {
					return true
				}
				_, _ = dst.Write(buffer[:count])
			}
		}
		if available == 0 {
			if quiet.done() {
				return false
			}
			select {
			case <-halt:
				return false
			case <-time.After(pumpInterval):
			}
			continue
		}
		size := min(int(available), len(buffer))
		var count uint32
		if err := windows.ReadFile(read, buffer[:size], &count, nil); err != nil {
			return true
		}
		_, _ = dst.Write(buffer[:count])
		quiet.reset()
	}
}

// holdOutputTimeout bounds how long an output holder waits for the next
// supervisor. By then the handoff has either completed or been rolled back.
const holdOutputTimeout = 2 * time.Minute

// spawnOutputHolder starts "spm-supervisor hold-output", which inherits the
// read end of the output pipe and keeps it open after this supervisor exits,
// until the next supervisor takes it (see takeOutput). The holder runs under
// this supervisor's account, as does the next supervisor, so taking the
// handle from it needs no rights beyond the account's own; a system service's
// account cannot open the elevated CLI that runs the handoff. An inherited
// handle keeps its value, which is returned with the holder's process ID.
func spawnOutputHolder(l Layout, name string, read windows.Handle, servicePID int) (int, uint64, error) {
	executable, err := os.Executable()
	if err != nil {
		return 0, 0, err
	}
	// A marker left by an earlier handoff must not release the new holder.
	_ = os.Remove(l.OutputTakenPath(name))
	if err := windows.SetHandleInformation(read, windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
		return 0, 0, fmt.Errorf("share the output pipe: %w", err)
	}
	defer windows.SetHandleInformation(read, windows.HANDLE_FLAG_INHERIT, 0)
	cmd := exec.Command(executable, "hold-output", "--root", l.Root, name, "--handle", strconv.FormatUint(uint64(read), 10), "--pid", strconv.Itoa(servicePID))
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags:              windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
		HideWindow:                 true,
		AdditionalInheritedHandles: []syscall.Handle{syscall.Handle(read)},
	}
	if err := cmd.Start(); err != nil {
		return 0, 0, fmt.Errorf("start the output holder: %w", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	return pid, uint64(read), nil
}

// HoldOutput is the output holder: it keeps the inherited pipe handle open
// until the next supervisor has taken it (it then names this process in
// OutputTakenPath), the service process servicePID has exited, or
// holdOutputTimeout passes.
func HoldOutput(l Layout, name string, handle uint64, servicePID int) error {
	defer windows.CloseHandle(windows.Handle(handle))
	service, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(servicePID))
	if err != nil {
		// The service is gone, and so is anyone to write to the pipe.
		return nil
	}
	defer windows.CloseHandle(service)
	me := strconv.Itoa(os.Getpid())
	deadline := time.Now().Add(holdOutputTimeout)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(l.OutputTakenPath(name)); err == nil && string(data) == me {
			_ = os.Remove(l.OutputTakenPath(name))
			return nil
		}
		if event, _ := windows.WaitForSingleObject(service, 50); event == windows.WAIT_OBJECT_0 {
			return nil
		}
	}
	events := openEvents(l, name)
	defer events.Close()
	events.Printf("no supervisor took the output of process %d within %s; its further output is not recorded", servicePID, holdOutputTimeout)
	return nil
}

// releaseOutputHolder tells the output holder that its pipe has been taken.
func releaseOutputHolder(l Layout, name string, holder int) {
	_ = common.WriteFileAtomic(l.OutputTakenPath(name), []byte(strconv.Itoa(holder)), 0o600)
}

// takeOutput duplicates the pipe's read end from the output holder that a
// detached supervisor left. It runs under the same account as the holder.
func takeOutput(holder int, handle uint64) (windows.Handle, error) {
	process, err := windows.OpenProcess(windows.PROCESS_DUP_HANDLE, false, uint32(holder))
	if err != nil {
		return 0, fmt.Errorf("open holder process %d: %w", holder, err)
	}
	defer windows.CloseHandle(process)
	var read windows.Handle
	if err := windows.DuplicateHandle(process, windows.Handle(handle), windows.CurrentProcess(), &read, 0, false, windows.DUPLICATE_SAME_ACCESS); err != nil {
		return 0, fmt.Errorf("take the output pipe from process %d: %w", holder, err)
	}
	if kind, err := windows.GetFileType(read); err != nil || kind != windows.FILE_TYPE_PIPE {
		_ = windows.CloseHandle(read)
		return 0, fmt.Errorf("handle %d in process %d is not the output pipe", handle, holder)
	}
	return read, nil
}
