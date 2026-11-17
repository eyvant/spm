//go:build windows

package common

import "golang.org/x/sys/windows"

// stillActive is the exit code GetExitCodeProcess reports for a running
// process (STILL_ACTIVE).
const stillActive = 259

// ProcessAlive reports whether a process with this ID is running. Opening it
// is not enough: an exited process stays openable for as long as any process
// on the system still holds a handle to it.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(handle)
	var code uint32
	if err := windows.GetExitCodeProcess(handle, &code); err != nil {
		return true
	}
	return code == stillActive
}
