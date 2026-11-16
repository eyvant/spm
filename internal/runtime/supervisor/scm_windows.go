//go:build windows

package supervisor

import (
	"time"

	"golang.org/x/sys/windows/svc"
)

// ServicePrefix prefixes every SCM service name that spm registers.
const ServicePrefix = "spm-"

func SCMName(name string) string { return ServicePrefix + name }

type scmHandler struct {
	layout Layout
	name   string
}

// RunService hosts one system service under the Service Control Manager.
func RunService(l Layout, name string) error {
	return svc.Run(SCMName(name), &scmHandler{layout: l, name: name})
}

func (handler *scmHandler) Execute(_ []string, requests <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	changes <- svc.Status{State: svc.StartPending, WaitHint: 15000}
	done := make(chan error, 1)
	go func() { done <- Supervise(handler.layout, handler.name) }()

	status := svc.Status{State: svc.StartPending, WaitHint: 15000}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	stopping := false
	for {
		select {
		case <-done:
			return false, 0
		case request := <-requests:
			switch request.Cmd {
			case svc.Interrogate:
				changes <- status
			case svc.Stop, svc.Shutdown:
				if !stopping {
					stopping = true
					status = svc.Status{State: svc.StopPending, WaitHint: 30000}
					changes <- status
					_ = handler.layout.WriteControl(handler.name, "stop")
				}
			}
		case <-ticker.C:
			if stopping {
				continue
			}
			state := handler.layout.ReadState(handler.name)
			if state.Status == "running" && state.PID > 0 && status.State != svc.Running {
				status = svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
				changes <- status
			}
		}
	}
}
