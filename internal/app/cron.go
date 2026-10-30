package app

import (
	"fmt"
	"strconv"
	"strings"
)

func cronToOnCalendar(expression string) (string, error) {
	if strings.ContainsAny(expression, "\r\n") {
		return "", fmt.Errorf("expression cannot contain a newline")
	}
	fields := strings.Fields(expression)
	if len(fields) != 5 {
		return "", fmt.Errorf("expected 5 fields, got %d", len(fields))
	}
	minute, err := convertCronField(fields[0], 0, 59, 0)
	if err != nil {
		return "", fmt.Errorf("minute: %w", err)
	}
	hour, err := convertCronField(fields[1], 0, 23, 0)
	if err != nil {
		return "", fmt.Errorf("hour: %w", err)
	}
	day, err := convertCronField(fields[2], 1, 31, 1)
	if err != nil {
		return "", fmt.Errorf("day of month: %w", err)
	}
	month, err := convertCronField(fields[3], 1, 12, 1)
	if err != nil {
		return "", fmt.Errorf("month: %w", err)
	}
	weekday, err := convertCronWeekday(fields[4])
	if err != nil {
		return "", fmt.Errorf("day of week: %w", err)
	}
	if fields[2] != "*" && weekday != "*" {
		return "", fmt.Errorf("day of month and day of week cannot both be restricted")
	}
	prefix := ""
	if weekday != "*" {
		prefix = weekday + " "
	}
	return fmt.Sprintf("%s*-%s-%s %s:%s:00", prefix, month, day, hour, minute), nil
}

func convertCronField(field string, minimum, maximum, stepStart int) (string, error) {
	parts := strings.Split(field, ",")
	converted := make([]string, 0, len(parts))
	for _, part := range parts {
		value, err := convertCronPart(part, minimum, maximum, stepStart)
		if err != nil {
			return "", err
		}
		converted = append(converted, value)
	}
	return strings.Join(converted, ","), nil
}

func convertCronPart(part string, minimum, maximum, stepStart int) (string, error) {
	if part == "*" {
		return "*", nil
	}
	if strings.HasPrefix(part, "*/") {
		step, err := strconv.Atoi(strings.TrimPrefix(part, "*/"))
		if err != nil || step < 1 || step > maximum-minimum+1 {
			return "", fmt.Errorf("invalid step %q", part)
		}
		return fmt.Sprintf("%02d/%d", stepStart, step), nil
	}
	if strings.Count(part, "-") == 1 {
		bounds := strings.SplitN(part, "-", 2)
		start, err := parseCronNumber(bounds[0], minimum, maximum)
		if err != nil {
			return "", err
		}
		end, err := parseCronNumber(bounds[1], minimum, maximum)
		if err != nil {
			return "", err
		}
		if start > end {
			return "", fmt.Errorf("range %q starts after it ends", part)
		}
		return fmt.Sprintf("%02d..%02d", start, end), nil
	}
	value, err := parseCronNumber(part, minimum, maximum)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%02d", value), nil
}

func parseCronNumber(raw string, minimum, maximum int) (int, error) {
	value, err := strconv.Atoi(raw)
	if err != nil || value < minimum || value > maximum {
		return 0, fmt.Errorf("value %q must be between %d and %d", raw, minimum, maximum)
	}
	return value, nil
}

func convertCronWeekday(field string) (string, error) {
	if field == "*" {
		return "*", nil
	}
	names := []string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}
	parts := strings.Split(field, ",")
	converted := make([]string, 0, len(parts))
	for _, part := range parts {
		if strings.Contains(part, "/") {
			return "", fmt.Errorf("weekday steps are not supported")
		}
		if strings.Count(part, "-") == 1 {
			bounds := strings.SplitN(part, "-", 2)
			start, err := parseCronNumber(bounds[0], 0, 7)
			if err != nil {
				return "", err
			}
			end, err := parseCronNumber(bounds[1], 0, 7)
			if err != nil {
				return "", err
			}
			if start > end {
				return "", fmt.Errorf("range %q starts after it ends", part)
			}
			if start == 0 && end == 7 {
				return "*", nil
			}
			if start == 0 && end > 0 {
				converted = append(converted, "Sun")
				if end == 1 {
					converted = append(converted, "Mon")
				} else {
					converted = append(converted, "Mon.."+names[end])
				}
			} else if start == end {
				converted = append(converted, names[start])
			} else {
				converted = append(converted, names[start]+".."+names[end])
			}
			continue
		}
		value, err := parseCronNumber(part, 0, 7)
		if err != nil {
			return "", err
		}
		converted = append(converted, names[value])
	}
	return strings.Join(converted, ","), nil
}

func renderCronRestartUnits(name, expression, timezone, calendar, systemctl string, system bool) (string, string) {
	systemctlArgs := ""
	if !system {
		systemctlArgs = " \"--user\""
	}
	service := "[Unit]\n" +
		"Description=" + escapeSystemdSpecifiers("Scheduled restart for spm service: "+name) + "\n\n" +
		"[Service]\n" +
		"Type=oneshot\n" +
		"ExecStart=" + quoteSystemdExec(systemctl) + systemctlArgs + " \"restart\" " + quoteSystemdExec(unitName(name)) + "\n"
	timezoneComment := ""
	if timezone != "" {
		timezoneComment = "# Timezone=" + timezone + "\n"
	}
	timer := "# CronExpression=" + expression + "\n" + timezoneComment +
		"[Unit]\n" +
		"Description=" + escapeSystemdSpecifiers("Cron restart timer for spm service: "+name) + "\n" +
		"StartLimitIntervalSec=0\n\n" +
		"[Timer]\n" +
		"OnCalendar=" + calendar + "\n" +
		"Unit=" + cronRestartServiceName(name) + "\n\n" +
		"[Install]\n" +
		"WantedBy=timers.target\n"
	return service, timer
}
