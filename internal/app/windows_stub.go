//go:build !windows

package app

import (
	"errors"
	"io"
)

func runWindows(_ []string, _, _ io.Writer, _ bool) error {
	return errors.New("windows backend is unavailable in this build")
}
