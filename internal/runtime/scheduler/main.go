package scheduler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
)

// Main is the spm-scheduler entry point:
//
//	spm-scheduler version
//	spm-scheduler daemon --root ROOT [--domain DOMAIN]
//	spm-scheduler run --config FILE --state-dir DIR [--log FILE] [--activation KIND] [--force]
func Main(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: spm-scheduler version | daemon --root ROOT [--domain DOMAIN] | run ...")
	}
	switch args[0] {
	case "version", "--version":
		fmt.Fprintf(stdout, "spm-scheduler %d\n", Version)
		return nil
	case "run":
		return Run(args[1:], stdout, stderr)
	case "daemon":
		root, domain, err := parseDaemonArgs(args[1:])
		if err != nil {
			return err
		}
		trigger, err := platformTrigger(domain)
		if err != nil {
			return err
		}
		log, closeLog := OpenDaemonLog(root, stderr)
		defer closeLog()
		return RunDaemon(context.Background(), root, trigger, log)
	default:
		return fmt.Errorf("unknown spm-scheduler command %q", args[0])
	}
}

func parseDaemonArgs(args []string) (string, string, error) {
	root, domain := "", ""
	for index := 0; index < len(args); index += 2 {
		if index+1 >= len(args) {
			return "", "", fmt.Errorf("%s requires a value", args[index])
		}
		switch args[index] {
		case "--root":
			root = args[index+1]
		case "--domain":
			domain = args[index+1]
		default:
			return "", "", fmt.Errorf("unknown scheduler daemon option %q", args[index])
		}
	}
	if !filepath.IsAbs(root) {
		return "", "", errors.New("scheduler daemon requires an absolute --root")
	}
	return filepath.Clean(root), domain, nil
}
