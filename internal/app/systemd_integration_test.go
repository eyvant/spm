package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestGeneratedUnitsPassSystemdAnalyzeVerify(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd-analyze verification requires Linux")
	}
	if _, err := exec.LookPath("systemd-analyze"); err != nil {
		t.Skip("systemd-analyze is unavailable")
	}
	directory := t.TempDir()
	options := defaultStartOptions()
	options.name = "verify"
	options.description = "spm verify"
	options.cwd = directory
	options.command = []string{"/bin/true"}
	options.startLimitInterval = "30s"
	options.startLimitBurst = "20"
	options.maxMemory = "64M"
	options.health = healthConfig{Probe: probeConfig{Kind: "http", Target: "http://127.0.0.1:3000/health", Timeout: "5s", Retries: 3, RetryDelay: "1s"}, Interval: "30s", StartPeriod: "10s", Action: "restart"}
	unit, err := renderUnit(options, false)
	if err != nil {
		t.Fatal(err)
	}
	service, timer := renderCronRestartUnits("verify", "0-5", "", "Sun,Mon..Fri *-*-* 09:00:00", "/usr/bin/systemctl", false)
	systemOptions := options
	systemOptions.name = "verify-system"
	systemOptions.runAsUser = "nobody"
	systemOptions.runAsGroup = "nogroup"
	systemOptions.execStartPre = []string{"/bin/true"}
	systemOptions.execStartPost = []string{"/bin/true"}
	systemOptions.execStop = "/bin/true"
	systemOptions.execReload = "/bin/true"
	systemOptions.onFailure = []string{"network.target"}
	systemUnit, err := renderUnit(systemOptions, true)
	if err != nil {
		t.Fatal(err)
	}
	systemService, _ := renderCronRestartUnits("verify-system", "0-5", "", "Sun,Mon..Fri *-*-* 09:00:00", "/usr/bin/systemctl", true)
	healthService, healthTimer, healthRecovery, err := renderHealthUnits(options, false)
	if err != nil {
		t.Fatal(err)
	}
	scheduleConfig := persistedStartConfig{Mode: "schedule", TimerOnBoot: "5min", TimerInterval: "30min", TimerRandomDelay: "2min"}
	scheduleTimer := renderScheduleTimer("verify", scheduleConfig, "")
	paths := map[string]string{
		unitName("verify"):                      unit,
		cronRestartServiceName("verify"):        service,
		cronRestartTimerName("verify"):          timer,
		unitName("verify-system"):               systemUnit,
		cronRestartServiceName("verify-system"): systemService,
		healthServiceName("verify"):             healthService,
		healthTimerName("verify"):               healthTimer,
		healthRecoveryName("verify"):            healthRecovery,
		scheduleTimerName("verify"):             scheduleTimer,
	}
	var verifyPaths []string
	for name, content := range paths {
		path := filepath.Join(directory, name)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		verifyPaths = append(verifyPaths, path)
	}
	args := append([]string{"verify"}, verifyPaths...)
	if output, err := exec.Command("systemd-analyze", args...).CombinedOutput(); err != nil {
		t.Fatalf("systemd-analyze verify: %v\n%s", err, output)
	}
}
