package supervisor

import "fmt"

// ExitStatus is returned when spm-supervisor must exit with a particular
// code: on macOS it exits as its service did, so that launchd applies the
// service's restart policy to the service's own exit status.
type ExitStatus struct{ Code int }

func (e ExitStatus) Error() string { return fmt.Sprintf("service exited with code %d", e.Code) }
