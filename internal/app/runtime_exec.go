package app

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

const (
	supervisorRuntime = "spm-supervisor"
	schedulerRuntime  = "spm-scheduler"
)

// runtimeExecutable returns the absolute path of a runtime binary
// (spm-supervisor or spm-scheduler). Installers put them next to spm;
// SPM_RUNTIME_DIR overrides the directory for development and tests.
func runtimeExecutable(name string) (string, error) {
	file := name
	if runtime.GOOS == "windows" {
		file += ".exe"
	}
	directory := os.Getenv("SPM_RUNTIME_DIR")
	if directory == "" {
		executable, err := os.Executable()
		if err != nil {
			return "", fmt.Errorf("resolve spm executable: %w", err)
		}
		if resolved, err := filepath.EvalSymlinks(executable); err == nil {
			executable = resolved
		}
		directory = filepath.Dir(executable)
	}
	path, err := filepath.Abs(filepath.Join(directory, file))
	if err != nil {
		return "", err
	}
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		return "", fmt.Errorf("%s was not found in %s; reinstall spm, or set SPM_RUNTIME_DIR to the directory that contains it", file, directory)
	}
	return path, nil
}
