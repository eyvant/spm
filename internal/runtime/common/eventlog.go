package common

import (
	"bytes"
	"fmt"
	"io"
	"sync"
	"time"
)

// File names below a service directory and the data root. A service's own
// output goes to ServiceLogName; what spm does with the service (starts,
// exits, scheduled runs, supervisor upgrades) goes to EventLogName, so the two
// never mix. The scheduler daemon writes SchedulerLogName in the data root.
const (
	ServiceLogName   = "service.log"
	EventLogName     = "spm.log"
	SchedulerLogName = "scheduler.log"
)

// EventLog is a rotating log that starts every line with a timestamp.
type EventLog struct {
	mu      sync.Mutex
	out     io.Writer
	close   func() error
	partial bool
}

// OpenEventLog opens path as a rotating event log. If it cannot be opened,
// lines go to fallback instead, so callers never have to check for nil.
func OpenEventLog(path string, fallback io.Writer) *EventLog {
	file, err := OpenRotatingLog(path, DefaultLogMaxBytes, DefaultLogBackups)
	if err != nil {
		if fallback == nil {
			fallback = io.Discard
		}
		return NewEventLog(fallback)
	}
	return &EventLog{out: file, close: file.Close}
}

// NewEventLog timestamps lines written to w.
func NewEventLog(w io.Writer) *EventLog {
	return &EventLog{out: w, close: func() error { return nil }}
}

// Printf writes one timestamped line.
func (log *EventLog) Printf(format string, args ...any) {
	_, _ = log.Write([]byte(fmt.Sprintf(format, args...) + "\n"))
}

// Write timestamps every line of p. A line split across writes gets one
// timestamp.
func (log *EventLog) Write(p []byte) (int, error) {
	log.mu.Lock()
	defer log.mu.Unlock()
	written := len(p)
	var buffer bytes.Buffer
	for len(p) > 0 {
		if !log.partial {
			buffer.WriteString("[" + time.Now().Format(time.RFC3339) + "] ")
		}
		line := p
		if index := bytes.IndexByte(p, '\n'); index >= 0 {
			line = p[:index+1]
		}
		buffer.Write(line)
		log.partial = line[len(line)-1] != '\n'
		p = p[len(line):]
	}
	if _, err := log.out.Write(buffer.Bytes()); err != nil {
		return 0, err
	}
	return written, nil
}

func (log *EventLog) Close() error {
	log.mu.Lock()
	defer log.mu.Unlock()
	return log.close()
}
