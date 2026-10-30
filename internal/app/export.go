package app

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

func (a *application) exportConfig(args []string) error {
	if len(args) != 1 {
		return errors.New("export requires one service name or all")
	}
	var names []string
	var err error
	if args[0] == "all" {
		names, err = a.managedNames()
	} else {
		if err := validateName(args[0]); err != nil {
			return err
		}
		names = []string{args[0]}
	}
	if err != nil {
		return err
	}
	for _, name := range names {
		config, err := a.readServiceConfig(name)
		if err != nil {
			return fmt.Errorf("export %s: %w", name, err)
		}
		writeDeclaration(a.stdout, name, config)
	}
	return nil
}

func writeDeclaration(w io.Writer, name string, config persistedStartConfig) {
	var builder strings.Builder
	builder.WriteString("[[services]]\nname = " + strconv.Quote(name) + "\n")
	writeTOMLString(&builder, "cwd", config.Cwd)
	writeTOMLString(&builder, "description", config.Description)
	command := config.Command
	if config.Mise && len(command) >= 4 && executableBase(command[0]) == "mise" && command[1] == "exec" && command[2] == "--" {
		command = command[3:]
	}
	builder.WriteString("command = " + stringArray(command) + "\n")
	writeTOMLString(&builder, "restart", config.Restart)
	writeTOMLString(&builder, "restart_sec", config.RestartSec)
	if config.Mode == "schedule" {
		writeTOMLString(&builder, "schedule", config.Schedule)
		writeTOMLString(&builder, "timer_on_boot", config.TimerOnBoot)
		writeTOMLString(&builder, "timer_interval", config.TimerInterval)
		writeTOMLString(&builder, "timer_randomized_delay", config.TimerRandomDelay)
		fmt.Fprintf(&builder, "persistent = %t\n", config.Persistent)
	} else {
		writeTOMLString(&builder, "cron_restart", config.CronRestart)
	}
	writeTOMLString(&builder, "env_file", config.EnvFile)
	writeTOMLString(&builder, "max_memory", config.MaxMemory)
	writeTOMLString(&builder, "user", config.User)
	writeTOMLString(&builder, "group", config.Group)
	writeTOMLString(&builder, "type", config.Type)
	writeTOMLString(&builder, "pid_file", config.PIDFile)
	writeTOMLString(&builder, "timezone", config.Timezone)
	if config.Instances > 0 {
		fmt.Fprintf(&builder, "instances = %d\n", config.Instances)
	}
	writeTOMLString(&builder, "exec_stop", config.ExecStop)
	writeTOMLString(&builder, "exec_reload", config.ExecReload)
	if config.Health.Probe.Kind != "" {
		writeTOMLString(&builder, "health_"+config.Health.Probe.Kind, config.Health.Probe.Target)
		writeTOMLString(&builder, "health_interval", config.Health.Interval)
		writeTOMLString(&builder, "health_timeout", config.Health.Probe.Timeout)
		if config.Health.Probe.Retries > 0 {
			fmt.Fprintf(&builder, "health_retries = %d\n", config.Health.Probe.Retries)
		}
		writeTOMLString(&builder, "health_retry_delay", config.Health.Probe.RetryDelay)
		writeTOMLString(&builder, "health_start_period", config.Health.StartPeriod)
		writeTOMLString(&builder, "health_action", config.Health.Action)
	}
	for _, field := range []struct {
		key    string
		values []string
	}{{"env", config.Env}, {"properties", config.Properties}, {"exec_start_pre", config.ExecStartPre}, {"exec_start_post", config.ExecStartPost}, {"after", config.After}, {"wants", config.Wants}, {"requires", config.Requires}, {"on_failure", config.OnFailure}} {
		if len(field.values) > 0 {
			builder.WriteString(field.key + " = " + stringArray(field.values) + "\n")
		}
	}
	fmt.Fprintf(&builder, "mise = %t\nenable = %t\n\n", config.Mise, config.Enable)
	_, _ = io.WriteString(w, builder.String())
}

func writeTOMLString(builder *strings.Builder, key, value string) {
	if value != "" {
		builder.WriteString(key + " = " + strconv.Quote(value) + "\n")
	}
}

func stringArray(values []string) string {
	quoted := make([]string, len(values))
	for i, value := range values {
		quoted[i] = strconv.Quote(value)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}
