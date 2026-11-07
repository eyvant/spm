package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
)

func main() {
	output := flag.String("o", "", "output coverage profile")
	flag.Parse()
	if *output == "" || flag.NArg() < 1 {
		fatal(errors.New("usage: merge-coverage -o OUTPUT PROFILE..."))
	}
	mode := ""
	order := make([]string, 0)
	counts := make(map[string]uint64)
	for _, path := range flag.Args() {
		file, err := os.Open(path)
		if err != nil {
			fatal(err)
		}
		scanner := bufio.NewScanner(file)
		if !scanner.Scan() || !strings.HasPrefix(scanner.Text(), "mode: ") {
			_ = file.Close()
			fatal(fmt.Errorf("%s is not a coverage profile", path))
		}
		profileMode := strings.TrimPrefix(scanner.Text(), "mode: ")
		if mode == "" {
			mode = profileMode
		} else if profileMode != mode {
			_ = file.Close()
			fatal(fmt.Errorf("coverage mode mismatch: %s uses %s, want %s", path, profileMode, mode))
		}
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			separator := strings.LastIndexByte(line, ' ')
			if separator < 0 {
				_ = file.Close()
				fatal(fmt.Errorf("invalid coverage line in %s: %s", path, line))
			}
			key := line[:separator]
			count, err := strconv.ParseUint(line[separator+1:], 10, 64)
			if err != nil {
				_ = file.Close()
				fatal(fmt.Errorf("invalid coverage count in %s: %w", path, err))
			}
			existing, exists := counts[key]
			if !exists {
				order = append(order, key)
				counts[key] = count
			} else if count > existing {
				counts[key] = count
			}
		}
		err = scanner.Err()
		closeErr := file.Close()
		if err != nil {
			fatal(err)
		}
		if closeErr != nil {
			fatal(closeErr)
		}
	}
	file, err := os.Create(*output)
	if err != nil {
		fatal(err)
	}
	writer := bufio.NewWriter(file)
	_, _ = fmt.Fprintf(writer, "mode: %s\n", mode)
	for _, key := range order {
		_, _ = fmt.Fprintf(writer, "%s %d\n", key, counts[key])
	}
	if err := writer.Flush(); err != nil {
		_ = file.Close()
		fatal(err)
	}
	if err := file.Close(); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	_, _ = fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
