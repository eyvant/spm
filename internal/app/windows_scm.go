//go:build windows

package app

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const windowsServicePrefix = "spm-"

func windowsIsElevated() bool { return windows.GetCurrentProcessToken().IsElevated() }

type windowsSCMHandler struct {
	backend *windowsBackend
	name    string
}

func runWindowsSCMService(backend *windowsBackend, name string) error {
	return svc.Run(backend.scmName(name), &windowsSCMHandler{backend: backend, name: name})
}

func (handler *windowsSCMHandler) Execute(_ []string, requests <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	changes <- svc.Status{State: svc.StartPending, WaitHint: 15000}
	done := make(chan error, 1)
	go func() { done <- handler.backend.supervise(handler.name) }()

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
					_ = handler.backend.writeControl(handler.name, "stop")
				}
			}
		case <-ticker.C:
			if stopping {
				continue
			}
			state := handler.backend.readState(handler.name)
			if state.Status == "running" && state.PID > 0 && status.State != svc.Running {
				status = svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
				changes <- status
			}
		}
	}
}

func (b *windowsBackend) scmName(name string) string { return windowsServicePrefix + name }

func (b *windowsBackend) scmInstalled(name string) bool {
	manager, err := mgr.Connect()
	if err != nil {
		return false
	}
	defer manager.Disconnect()
	service, err := manager.OpenService(b.scmName(name))
	if err != nil {
		return false
	}
	_ = service.Close()
	return true
}

func (b *windowsBackend) installSCMService(name string) error {
	config, err := b.readConfig(name)
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve spm executable: %w", err)
	}
	stableExecutable := filepath.Join(b.serviceDir(name), "spm-supervisor.exe")
	data, err := os.ReadFile(executable)
	if err != nil {
		return fmt.Errorf("read spm executable for system service: %w", err)
	}
	if err := writeFileAtomic(stableExecutable, data, 0o700); err != nil {
		return fmt.Errorf("install system supervisor executable: %w", err)
	}

	manager, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to Windows Service Control Manager: %w", err)
	}
	defer manager.Disconnect()
	if existing, openErr := manager.OpenService(b.scmName(name)); openErr == nil {
		_ = existing.Close()
		return b.configureSCMStartType(name, config.Enabled)
	}

	account := config.RunAs
	password := ""
	if account == "SYSTEM" || account == "" {
		account = ""
	} else if windowsVirtualServiceAccount(account) {
		expected := `NT SERVICE\` + b.scmName(name)
		if !strings.EqualFold(account, expected) {
			return fmt.Errorf("virtual account for %s must be %q", name, expected)
		}
	} else if !windowsBuiltInAccount(account) && !windowsVirtualServiceAccount(account) {
		if err := grantWindowsServiceLogon(account); err != nil {
			return fmt.Errorf("grant Log on as a service to %s: %w", account, err)
		}
		if !strings.HasSuffix(account, "$") {
			password, err = b.promptWindowsServicePassword(account)
			if err != nil {
				return err
			}
		}
	}
	startType := uint32(mgr.StartManual)
	if config.Enabled {
		startType = mgr.StartAutomatic
	}
	dependencies := make([]string, 0, len(config.Requires))
	for _, dependency := range config.Requires {
		dependencies = append(dependencies, b.scmName(dependency))
	}
	description := config.Description
	if description == "" {
		description = "Managed by spm"
	}
	service, err := manager.CreateService(
		b.scmName(name), stableExecutable,
		mgr.Config{
			StartType:        startType,
			ErrorControl:     mgr.ErrorNormal,
			Dependencies:     dependencies,
			ServiceStartName: account,
			Password:         password,
			DisplayName:      "spm: " + name,
			Description:      description,
		},
		"__windows-service", "--root", b.root, name,
	)
	password = ""
	if err != nil {
		return fmt.Errorf("create Windows service %s: %w", b.scmName(name), err)
	}
	defer service.Close()
	if err := service.SetRecoveryActions([]mgr.RecoveryAction{{Type: mgr.ServiceRestart, Delay: 5 * time.Second}}, 86400); err != nil {
		_ = service.Delete()
		return fmt.Errorf("configure Windows service recovery: %w", err)
	}
	return nil
}

func windowsVirtualServiceAccount(account string) bool {
	return strings.HasPrefix(strings.ToUpper(strings.TrimSpace(account)), `NT SERVICE\`)
}

func (b *windowsBackend) promptWindowsServicePassword(account string) (string, error) {
	fmt.Fprintf(b.stderr, "Enter the password for Windows service account %s. It will be passed directly to SCM and is not stored by spm.\n", account)
	const script = `$ErrorActionPreference = 'Stop'
$secure = Read-Host -Prompt "Password for $env:SPM_SERVICE_ACCOUNT" -AsSecureString
$pointer = [Runtime.InteropServices.Marshal]::SecureStringToBSTR($secure)
try {
    [Console]::Out.Write([Runtime.InteropServices.Marshal]::PtrToStringBSTR($pointer))
} finally {
    if ($pointer -ne [IntPtr]::Zero) { [Runtime.InteropServices.Marshal]::ZeroFreeBSTR($pointer) }
}`
	cmd := exec.Command("powershell.exe", "-NoLogo", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", script)
	cmd.Env = append(os.Environ(), "SPM_SERVICE_ACCOUNT="+account)
	cmd.Stdin, cmd.Stderr = os.Stdin, b.stderr
	var output bytes.Buffer
	cmd.Stdout = &output
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("read Windows service account password: %w", err)
	}
	password := output.String()
	if password == "" {
		return "", errors.New("windows service account password cannot be empty")
	}
	return password, nil
}

func (b *windowsBackend) configureSCMStartType(name string, enabled bool) error {
	manager, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to Windows Service Control Manager: %w", err)
	}
	defer manager.Disconnect()
	service, err := manager.OpenService(b.scmName(name))
	if err != nil {
		return fmt.Errorf("open Windows service %s: %w", b.scmName(name), err)
	}
	defer service.Close()
	config, err := service.Config()
	if err != nil {
		return fmt.Errorf("read Windows service configuration: %w", err)
	}
	config.StartType = mgr.StartManual
	if enabled {
		config.StartType = mgr.StartAutomatic
	}
	config.Password = ""
	if err := service.UpdateConfig(config); err != nil {
		return fmt.Errorf("update Windows service startup type: %w", err)
	}
	return nil
}

func (b *windowsBackend) startSCMService(name string) error {
	manager, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to Windows Service Control Manager: %w", err)
	}
	defer manager.Disconnect()
	service, err := manager.OpenService(b.scmName(name))
	if err != nil {
		return fmt.Errorf("open Windows service %s: %w", b.scmName(name), err)
	}
	defer service.Close()
	status, err := service.Query()
	if err != nil {
		return fmt.Errorf("query Windows service: %w", err)
	}
	if status.State == svc.Running {
		return nil
	}
	if err := service.Start(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
		return fmt.Errorf("start Windows service: %w", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		status, err = service.Query()
		if err != nil {
			return err
		}
		if status.State == svc.Running {
			return b.waitRunning(name, 10*time.Second)
		}
		if status.State == svc.Stopped {
			return fmt.Errorf("windows service %s stopped during startup (win32=%d, service=%d)", b.scmName(name), status.Win32ExitCode, status.ServiceSpecificExitCode)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("timed out starting Windows service %s", b.scmName(name))
}

func (b *windowsBackend) stopSCMService(name string) error {
	manager, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to Windows Service Control Manager: %w", err)
	}
	defer manager.Disconnect()
	service, err := manager.OpenService(b.scmName(name))
	if err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return nil
		}
		return err
	}
	defer service.Close()
	status, err := service.Query()
	if err != nil {
		return err
	}
	if status.State == svc.Stopped {
		return nil
	}
	if _, err := service.Control(svc.Stop); err != nil && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
		return fmt.Errorf("stop Windows service: %w", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		status, err = service.Query()
		if err != nil {
			return err
		}
		if status.State == svc.Stopped {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("timed out stopping Windows service %s", b.scmName(name))
}

func (b *windowsBackend) deleteSCMService(name string) error {
	manager, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to Windows Service Control Manager: %w", err)
	}
	defer manager.Disconnect()
	service, err := manager.OpenService(b.scmName(name))
	if err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return b.removeLegacySystemTask(name)
		}
		return err
	}
	defer service.Close()
	if err := service.Delete(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
		return fmt.Errorf("delete Windows service: %w", err)
	}
	return b.removeLegacySystemTask(name)
}

func (b *windowsBackend) stopLegacySystemTask(name string) error {
	state := b.readState(name)
	if state.SupervisorPID > 0 && windowsProcessAlive(state.SupervisorPID) {
		if err := b.sendControl(name, "stop"); err != nil {
			return fmt.Errorf("stop legacy Task Scheduler supervisor: %w", err)
		}
	}
	return b.removeLegacySystemTask(name)
}

func (b *windowsBackend) removeLegacySystemTask(name string) error {
	taskName := "spm-system-" + name
	query := exec.Command("schtasks.exe", "/Query", "/TN", taskName)
	query.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := query.Run(); err != nil {
		return nil
	}
	cmd := exec.Command("schtasks.exe", "/Delete", "/TN", taskName, "/F")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("remove legacy scheduled task: %s: %w", strings.TrimSpace(string(output)), err)
	}
	return nil
}

func (b *windowsBackend) inspectSCMService(name string, stdout, stderr io.Writer) error {
	for _, command := range [][]string{{"queryex", b.scmName(name)}, {"qc", b.scmName(name)}} {
		cmd := exec.Command("sc.exe", command...)
		cmd.Stdout, cmd.Stderr = stdout, stderr
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("sc.exe %s: %w", command[0], err)
		}
	}
	return nil
}

func (b *windowsBackend) checkSCM() error {
	manager, err := mgr.Connect()
	if err != nil {
		return err
	}
	return manager.Disconnect()
}

type windowsLSAObjectAttributes struct {
	Length                   uint32
	RootDirectory            uintptr
	ObjectName               uintptr
	Attributes               uint32
	SecurityDescriptor       uintptr
	SecurityQualityOfService uintptr
}

type windowsLSAUnicodeString struct {
	Length        uint16
	MaximumLength uint16
	Buffer        *uint16
}

func grantWindowsServiceLogon(account string) error {
	accountUTF16, err := syscall.UTF16PtrFromString(account)
	if err != nil {
		return err
	}
	var sidLength, domainLength, accountUse uint32
	err = windows.LookupAccountName(nil, accountUTF16, nil, &sidLength, nil, &domainLength, &accountUse)
	if !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) || sidLength == 0 {
		return fmt.Errorf("look up account: %w", err)
	}
	sidBuffer := make([]byte, sidLength)
	domainBuffer := make([]uint16, domainLength)
	sid := (*windows.SID)(unsafe.Pointer(&sidBuffer[0]))
	var domain *uint16
	if len(domainBuffer) > 0 {
		domain = &domainBuffer[0]
	}
	if err := windows.LookupAccountName(nil, accountUTF16, sid, &sidLength, domain, &domainLength, &accountUse); err != nil {
		return fmt.Errorf("look up account: %w", err)
	}

	advapi := windows.NewLazySystemDLL("advapi32.dll")
	openPolicy := advapi.NewProc("LsaOpenPolicy")
	addRights := advapi.NewProc("LsaAddAccountRights")
	closePolicy := advapi.NewProc("LsaClose")
	statusToError := advapi.NewProc("LsaNtStatusToWinError")
	attributes := windowsLSAObjectAttributes{Length: uint32(unsafe.Sizeof(windowsLSAObjectAttributes{}))}
	var policy uintptr
	status, _, _ := openPolicy.Call(0, uintptr(unsafe.Pointer(&attributes)), 0x00000810, uintptr(unsafe.Pointer(&policy))) // POLICY_LOOKUP_NAMES | POLICY_CREATE_ACCOUNT
	if status != 0 {
		return windowsNTStatusError(statusToError, status)
	}
	defer closePolicy.Call(policy)
	rightBuffer, err := syscall.UTF16FromString("SeServiceLogonRight")
	if err != nil {
		return err
	}
	right := windowsLSAUnicodeString{
		Length:        uint16((len(rightBuffer) - 1) * 2),
		MaximumLength: uint16(len(rightBuffer) * 2),
		Buffer:        &rightBuffer[0],
	}
	status, _, _ = addRights.Call(policy, uintptr(unsafe.Pointer(sid)), uintptr(unsafe.Pointer(&right)), 1)
	if status != 0 {
		return windowsNTStatusError(statusToError, status)
	}
	return nil
}

func windowsNTStatusError(converter *windows.LazyProc, status uintptr) error {
	code, _, _ := converter.Call(status)
	if code == 0 {
		return fmt.Errorf("NTSTATUS 0x%x", status)
	}
	return syscall.Errno(code)
}
