package app

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/eyvant/spm/internal/runtime/common"
)

// logFile is one log `spm logs` reads on Windows and macOS, where spm keeps
// logs in files: a service's output (service.log), what spm recorded about
// it (spm.log), or the scheduler daemon's log (scheduler.log).
type logFile struct {
	label string
	path  string
}

// fileLogBackend is what selectLogFiles needs from a file-based backend.
type fileLogBackend interface {
	names() ([]string, error)
	exists(name string) bool
	logPath(name string) string
	eventLogPath(name string) string
	schedulerLogPath() string
}

func selectLogFiles(b fileLogBackend, options logsOptions, platform string) ([]logFile, error) {
	if options.since != "" || options.until != "" || options.output != "" || options.boot || options.health {
		return nil, fmt.Errorf("%s logs support service names, --lines, --follow, --grep, --spm and --scheduler", platform)
	}
	if options.scheduler {
		return []logFile{{label: "scheduler", path: b.schedulerLogPath()}}, nil
	}
	names := options.names
	if len(names) == 0 || len(names) == 1 && names[0] == "all" {
		var err error
		if names, err = b.names(); err != nil {
			return nil, err
		}
	}
	var files []logFile
	for _, name := range names {
		if !b.exists(name) {
			return nil, fmt.Errorf("service %q is not managed by spm", name)
		}
		path := b.logPath(name)
		if options.spm {
			path = b.eventLogPath(name)
		}
		files = append(files, logFile{label: name, path: path})
	}
	return files, nil
}

// printLogFiles prints the last lines of each file and, with --follow, keeps
// printing what is appended, across rotations. With more than one file every
// line starts with the service name.
func printLogFiles(w io.Writer, files []logFile, options logsOptions) error {
	count, _ := strconv.Atoi(options.lines)
	grep, err := common.CompileLogGrep(options.grep)
	if err != nil {
		return err
	}
	writers := make([]io.Writer, len(files))
	for index, file := range files {
		writers[index] = w
		if len(files) > 1 {
			writers[index] = &linePrefixWriter{out: w, prefix: file.label + " | "}
		}
		if err := common.PrintRotatedLogTail(writers[index], file.path, count, grep); err != nil {
			return err
		}
	}
	if !options.follow {
		return nil
	}
	offsets := make([]int64, len(files))
	identities := make([]os.FileInfo, len(files))
	for index, file := range files {
		if info, err := os.Stat(file.path); err == nil {
			offsets[index], identities[index] = info.Size(), info
		}
	}
	for {
		time.Sleep(500 * time.Millisecond)
		for index, file := range files {
			info, offset, err := common.ReadRotatingLogUpdates(writers[index], file.path, identities[index], offsets[index], grep)
			if err == nil {
				identities[index], offsets[index] = info, offset
			}
		}
	}
}

// linePrefixWriter starts every line with prefix.
type linePrefixWriter struct {
	out     io.Writer
	prefix  string
	partial bool
}

func (w *linePrefixWriter) Write(p []byte) (int, error) {
	var buffer bytes.Buffer
	for rest := p; len(rest) > 0; {
		if !w.partial {
			buffer.WriteString(w.prefix)
		}
		line := rest
		if index := bytes.IndexByte(rest, '\n'); index >= 0 {
			line = rest[:index+1]
		}
		buffer.Write(line)
		w.partial = line[len(line)-1] != '\n'
		rest = rest[len(line):]
	}
	if _, err := w.out.Write(buffer.Bytes()); err != nil {
		return 0, err
	}
	return len(p), nil
}
