//go:build !darwin

package app

import (
	"errors"
	"io"
)

func runDarwin(_ []string, _, _ io.Writer, _ bool) error {
	return errors.New("macOS backend is unavailable in this build")
}
