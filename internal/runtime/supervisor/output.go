package supervisor

import "time"

const (
	// pumpInterval is how long the output copy waits for new output before
	// checking for a halt or drain request again.
	pumpInterval = 50 * time.Millisecond
	// drainQuiet is how long the pipe must stay empty, once a drain is
	// requested, before the copy stops. A process's last output can reach
	// the pipe a little after the process is reported as exited.
	drainQuiet = 250 * time.Millisecond
)

// quietTimer tracks, once drain closes, how long no output has arrived.
type quietTimer struct {
	drain <-chan struct{}
	since time.Time
}

func newQuietTimer(drain <-chan struct{}) *quietTimer { return &quietTimer{drain: drain} }

// reset records that output just arrived.
func (q *quietTimer) reset() { q.since = time.Time{} }

// done reports whether a drain was requested and the pipe has been empty for
// drainQuiet since.
func (q *quietTimer) done() bool {
	select {
	case <-q.drain:
	default:
		return false
	}
	if q.since.IsZero() {
		q.since = time.Now()
	}
	return time.Since(q.since) >= drainQuiet
}
