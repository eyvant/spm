//go:build windows

package app

func portableProcessAlive(pid int) bool {
	return windowsProcessAlive(pid)
}
