package common

import (
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// WriteFileAtomic replaces path with data through a temporary file and rename.
func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".spm-unit-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryName, path); err != nil {
		if runtime.GOOS != "windows" {
			return err
		}
		// On Windows, replacing a file fails with "Access is denied" while
		// another process has it open, for example a CLI polling a service
		// state file. Such readers hold the file only briefly, so retry
		// before falling back to remove-and-rename.
		for attempt := 0; attempt < atomicRenameRetries && err != nil; attempt++ {
			time.Sleep(atomicRenameRetryDelay)
			err = os.Rename(temporaryName, path)
		}
		if err == nil {
			return nil
		}
		if removeErr := os.Remove(path); removeErr != nil && !os.IsNotExist(removeErr) {
			return err
		}
		return os.Rename(temporaryName, path)
	}
	return nil
}

const (
	atomicRenameRetries    = 100
	atomicRenameRetryDelay = 10 * time.Millisecond
)
