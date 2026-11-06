package app

import (
	"fmt"
	"strconv"
	"strings"
)

func cronToOnCalendar(expression string) (string, error) {
	calendars, err := cronToOnCalendars(expression)
	if err != nil {
		return "", err
	}
	return strings.Join(calendars, "\nOnCalendar="), nil
}

func cronToOnCalendars(expression string) ([]string, error) {
	if strings.ContainsAny(expression, "\r\n") {
		return nil, fmt.Errorf("expression cannot contain a newline")
	}
	fields := strings.Fields(expression)
	if len(fields) != 5 && len(fields) != 6 {
		return nil, fmt.Errorf("expected 5 or 6 fields, got %d", len(fields))
	}
	if _, err := parseCronExpression(expression); err != nil {
		return nil, err
	}
	offset := 0
	second := "00"
	var err error
	if len(fields) == 6 {
		offset = 1
		second, err = convertCronField(fields[0], 0, 59, 0)
		if err != nil {
			return nil, fmt.Errorf("second: %w", err)
		}
	}
	minute, err := convertCronField(fields[offset], 0, 59, 0)
	if err != nil {
		return nil, fmt.Errorf("minute: %w", err)
	}
	hour, err := convertCronField(fields[offset+1], 0, 23, 0)
	if err != nil {
		return nil, fmt.Errorf("hour: %w", err)
	}
	day, err := convertCronField(fields[offset+2], 1, 31, 1)
	if err != nil {
		return nil, fmt.Errorf("day of month: %w", err)
	}
	month, err := convertCronField(fields[offset+3], 1, 12, 1)
	if err != nil {
		return nil, fmt.Errorf("month: %w", err)
	}
	weekday, err := convertCronWeekday(fields[offset+4])
	if err != nil {
		return nil, fmt.Errorf("day of week: %w", err)
	}
	render := func(renderedDay, renderedWeekday string) string {
		prefix := ""
		if renderedWeekday != "*" {
			prefix = renderedWeekday + " "
		}
		return fmt.Sprintf("%s*-%s-%s %s:%s:%s", prefix, month, renderedDay, hour, minute, second)
	}
	if fields[offset+2] != "*" && fields[offset+4] != "*" {
		return []string{render(day, "*"), render("*", weekday)}, nil
	}
	return []string{render(day, weekday)}, nil
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
	base, stepText, hasStep := strings.Cut(part, "/")
	stepSuffix := ""
	if hasStep {
		if strings.Contains(stepText, "/") {
			return "", fmt.Errorf("invalid step %q", part)
		}
		step, err := strconv.Atoi(stepText)
		if err != nil || step < 1 || step > maximum-minimum+1 {
			return "", fmt.Errorf("invalid step %q", part)
		}
		stepSuffix = "/" + strconv.Itoa(step)
	}
	if strings.Count(base, "-") == 1 {
		bounds := strings.SplitN(base, "-", 2)
		start, err := parseCronNumber(bounds[0], minimum, maximum)
		if err != nil {
			return "", err
		}
		end, err := parseCronNumber(bounds[1], minimum, maximum)
		if err != nil {
			return "", err
		}
		if start > end {
			return "", fmt.Errorf("range %q starts after it ends", base)
		}
		return fmt.Sprintf("%02d..%02d%s", start, end, stepSuffix), nil
	}
	if hasStep {
		return "", fmt.Errorf("step %q requires * or a range", part)
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
			parsed, err := parsePortableCronField(part, 0, 7, true)
			if err != nil {
				return "", err
			}
			for value, set := range parsed.values {
				if set {
					converted = append(converted, names[value])
				}
			}
			continue
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
		"AccuracySec=1s\n" +
		"OnCalendar=" + calendar + "\n" +
		"Unit=" + cronRestartServiceName(name) + "\n\n" +
		"[Install]\n" +
		"WantedBy=timers.target\n"
	return service, timer
}
