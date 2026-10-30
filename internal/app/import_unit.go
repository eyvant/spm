package app

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func (a *application) importUnit(args []string) error {
	if len(args) != 1 && len(args) != 3 {
		return errors.New("import requires UNIT-FILE [--name NAME]")
	}
	path := args[0]
	name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "spm-"), ".service")
	if len(args) == 3 {
		if args[1] != "--name" {
			return fmt.Errorf("unknown import option %q", args[1])
		}
		name = args[2]
	}
	if err := validateName(name); err != nil {
		return err
	}
	if a.exists(name) {
		return fmt.Errorf("service %q already exists", name)
	}
	config, err := parseSystemdUnit(path)
	if err != nil {
		return err
	}
	return a.installConfig(name, config, false)
}

func parseSystemdUnit(path string) (persistedStartConfig, error) {
	file, err := os.Open(path)
	if err != nil {
		return persistedStartConfig{}, err
	}
	defer file.Close()
	config := persistedStartConfig{Version: 1, Cwd: "/", Restart: "no", RestartSec: "100ms", Enable: true, ExplicitCmd: true}
	section := ""
	scanner := bufio.NewScanner(file)
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return config, fmt.Errorf("%s:%d: unsupported continuation or malformed directive", path, lineNumber)
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		switch section + "." + key {
		case "Unit.Description":
			config.Description = value
		case "Unit.After":
			config.After = append(config.After, strings.Fields(value)...)
		case "Unit.Wants":
			config.Wants = append(config.Wants, strings.Fields(value)...)
		case "Unit.Requires":
			config.Requires = append(config.Requires, strings.Fields(value)...)
		case "Unit.OnFailure":
			config.OnFailure = append(config.OnFailure, strings.Fields(value)...)
		case "Service.Type":
			supported := false
			for _, serviceType := range serviceTypes {
				supported = supported || value == serviceType
			}
			if !supported {
				return config, fmt.Errorf("%s:%d: unsupported service Type=%s", path, lineNumber, value)
			}
			if value != "exec" {
				config.Type = value
			}
		case "Service.PIDFile":
			config.PIDFile = value
		case "Service.ExecStart":
			if len(config.Command) != 0 {
				return config, fmt.Errorf("%s:%d: multiple ExecStart directives are unsupported", path, lineNumber)
			}
			config.Command, err = splitSystemdCommand(value)
			if err != nil {
				return config, fmt.Errorf("%s:%d: %w", path, lineNumber, err)
			}
		case "Service.WorkingDirectory":
			config.Cwd = value
		case "Service.Restart":
			config.Restart = value
		case "Service.RestartSec":
			config.RestartSec = value
		case "Service.User":
			config.User = value
		case "Service.Group":
			config.Group = value
		case "Service.ExecStartPre":
			config.ExecStartPre = append(config.ExecStartPre, value)
		case "Service.ExecStartPost":
			config.ExecStartPost = append(config.ExecStartPost, value)
		case "Service.ExecStop":
			if config.ExecStop != "" {
				return config, fmt.Errorf("%s:%d: multiple ExecStop directives are unsupported", path, lineNumber)
			}
			config.ExecStop = value
		case "Service.ExecReload":
			if config.ExecReload != "" {
				return config, fmt.Errorf("%s:%d: multiple ExecReload directives are unsupported", path, lineNumber)
			}
			config.ExecReload = value
		case "Service.Environment":
			values, parseErr := splitSystemdCommand(value)
			if parseErr != nil {
				return config, parseErr
			}
			config.Env = append(config.Env, values...)
		case "Service.EnvironmentFile":
			if strings.HasPrefix(value, "-") {
				return config, fmt.Errorf("%s:%d: optional EnvironmentFile is unsupported", path, lineNumber)
			}
			config.EnvFile = value
		case "Service.MemoryMax":
			config.MaxMemory = value
		case "Install.WantedBy":
			// spm regenerates the appropriate user or system install target.
		default:
			if section != "Service" {
				return config, fmt.Errorf("%s:%d: unsupported directive %s.%s", path, lineNumber, section, key)
			}
			property := key + "=" + value
			if err := validateServiceProperty(property); err != nil {
				return config, fmt.Errorf("%s:%d: cannot preserve %s: %w", path, lineNumber, key, err)
			}
			config.Properties = append(config.Properties, property)
		}
	}
	if err := scanner.Err(); err != nil {
		return config, err
	}
	if len(config.Command) == 0 {
		return config, errors.New("unit has no supported ExecStart")
	}
	return config, nil
}

func splitSystemdCommand(value string) ([]string, error) {
	var result []string
	var current strings.Builder
	quoted, escaped := false, false
	flush := func() {
		if current.Len() > 0 {
			result = append(result, current.String())
			current.Reset()
		}
	}
	for i := 0; i < len(value); i++ {
		char := value[i]
		if escaped {
			if char == 'x' && i+2 < len(value) {
				decoded, err := strconv.ParseUint(value[i+1:i+3], 16, 8)
				if err != nil {
					return nil, err
				}
				current.WriteByte(byte(decoded))
				i += 2
			} else {
				current.WriteByte(char)
			}
			escaped = false
			continue
		}
		if char == '\\' {
			escaped = true
			continue
		}
		if char == '"' {
			quoted = !quoted
			continue
		}
		if (char == ' ' || char == '\t') && !quoted {
			flush()
			continue
		}
		current.WriteByte(char)
	}
	if quoted || escaped {
		return nil, errors.New("unterminated quote or escape in command")
	}
	flush()
	if len(result) == 0 {
		return nil, errors.New("empty command")
	}
	if strings.ContainsAny(result[0][:1], "-+!@:") {
		return nil, errors.New("ExecStart prefixes and systemd specifiers are unsupported")
	}
	for _, arg := range result {
		if strings.Contains(arg, "%") {
			return nil, errors.New("systemd specifiers in ExecStart are unsupported")
		}
	}
	return result, nil
}
