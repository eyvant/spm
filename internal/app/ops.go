package app

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const maxInstances = 256

// normalizeSignal accepts SIGUSR2, USR2, usr2, or a number and returns the
// form passed to `systemctl kill --signal`.
func normalizeSignal(value string) (string, error) {
	if number, err := strconv.Atoi(value); err == nil {
		if number < 1 || number > 64 {
			return "", fmt.Errorf("signal number %d must be between 1 and 64", number)
		}
		return strconv.Itoa(number), nil
	}
	upper := strings.ToUpper(value)
	if !strings.HasPrefix(upper, "SIG") {
		upper = "SIG" + upper
	}
	name := strings.TrimPrefix(upper, "SIG")
	if name == "" {
		return "", fmt.Errorf("invalid signal %q", value)
	}
	for _, r := range name {
		if !(r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '+' || r == '-') {
			return "", fmt.Errorf("invalid signal %q", value)
		}
	}
	return upper, nil
}

func (a *application) signal(args []string) error {
	var positionals []string
	whom := "main"
	for _, arg := range args {
		switch arg {
		case "--all-processes":
			whom = "all"
		default:
			if strings.HasPrefix(arg, "--") {
				return fmt.Errorf("unknown signal option %q", arg)
			}
			positionals = append(positionals, arg)
		}
	}
	if len(positionals) < 2 {
		return errors.New("signal requires SIGNAL and at least one service name or all")
	}
	signal, err := normalizeSignal(positionals[0])
	if err != nil {
		return err
	}
	names, err := a.resolveServiceNames(positionals[1:])
	if err != nil {
		return err
	}
	if len(names) == 0 {
		fmt.Fprintln(a.stdout, "No services managed by spm.")
		return nil
	}
	var errs []error
	for _, name := range names {
		if !a.exists(name) {
			errs = append(errs, fmt.Errorf("%s: service is not managed by spm", name))
			continue
		}
		units := a.serviceUnits(name)
		callArgs := append([]string{"kill", "--signal=" + signal, "--kill-whom=" + whom}, units...)
		if _, err := a.runSystemctl(callArgs...); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		fmt.Fprintf(a.stdout, "%s sent %s\n", name, signal)
	}
	return errors.Join(errs...)
}

func parseInstanceCount(value string) (int, error) {
	count, err := strconv.Atoi(value)
	if err != nil || count < 0 || count > maxInstances {
		return 0, fmt.Errorf("instance count %q must be between 0 and %d", value, maxInstances)
	}
	return count, nil
}

// scale changes the number of running instances without regenerating the
// template, so existing instances keep running.
func (a *application) scale(args []string) error {
	if len(args) != 2 {
		return errors.New("scale requires a service name and an instance count")
	}
	name := args[0]
	if err := validateName(name); err != nil {
		return err
	}
	count, err := parseInstanceCount(args[1])
	if err != nil {
		return err
	}
	if count == 0 {
		return errors.New("scale requires at least 1 instance; use `spm stop` or `spm delete` instead")
	}
	if !a.exists(name) {
		return fmt.Errorf("service %q is not managed by spm", name)
	}
	if !a.isTemplate(name) {
		return fmt.Errorf("service %q was not started with --instances; replace it with `spm start ... --force --instances %d`", name, count)
	}
	config, err := a.readServiceConfig(name)
	if err != nil {
		return fmt.Errorf("read saved service config: %w", err)
	}
	current := a.instanceCount(name)
	if count == current {
		fmt.Fprintf(a.stdout, "%s already runs %d instances\n", name, count)
		return nil
	}
	if count > current {
		added := instanceUnits(name, count)[current:]
		action := []string{"start"}
		if config.Enable {
			action = []string{"enable", "--now"}
		}
		if _, err := a.runSystemctl(append(action, added...)...); err != nil {
			return fmt.Errorf("start new instances of %s: %w", name, err)
		}
		for _, unit := range added {
			if err := a.checkUnitStarted(unit, config.Restart); err != nil {
				return err
			}
		}
	} else {
		removed := instanceUnits(name, current)[count:]
		if _, err := a.runSystemctl(append([]string{"disable", "--now"}, removed...)...); err != nil {
			return fmt.Errorf("stop removed instances of %s: %w", name, err)
		}
		_, _ = a.runSystemctl(append([]string{"reset-failed"}, removed...)...)
	}
	config.Instances = count
	if err := a.writeServiceConfig(name, config); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "%s scaled from %d to %d instances\n", name, current, count)
	return nil
}
