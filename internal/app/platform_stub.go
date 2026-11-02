//go:build !windows && !darwin

package app

import "io"

func printPlatformHelp(io.Writer) bool { return false }

func printPlatformCommandHelp(io.Writer, string) bool { return false }

func platformCompletionApplication(io.Writer, bool) (*application, bool) { return nil, false }
