// Package sourcelock ties a runtime's Version to the source it is built from.
//
// Installers replace spm-supervisor and spm-scheduler, and restart what runs
// them, only when their Version changes. A code change without a version bump
// would therefore never reach running systems. Each runtime package records
// "VERSION SHA256" lines in source.lock, and its test fails when the current
// sources do not match the hash recorded for the current Version.
package sourcelock

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

var update = flag.Bool("update-source-lock", false, "record the source hash for a new runtime Version")

// Hash hashes the non-test Go files of the given directories. Line endings are
// normalized so checkouts with CRLF produce the same hash.
func Hash(directories ...string) (string, error) {
	var files []string
	for _, directory := range directories {
		matches, err := filepath.Glob(filepath.Join(directory, "*.go"))
		if err != nil {
			return "", err
		}
		for _, match := range matches {
			if !strings.HasSuffix(match, "_test.go") {
				files = append(files, match)
			}
		}
	}
	sort.Slice(files, func(i, j int) bool { return filepath.ToSlash(files[i]) < filepath.ToSlash(files[j]) })
	digest := sha256.New()
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(digest, "%s\n", filepath.ToSlash(file))
		digest.Write([]byte(strings.ReplaceAll(string(data), "\r\n", "\n")))
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// Check verifies that lockPath records the current hash for version.
func Check(t *testing.T, lockPath string, version int, directories ...string) {
	t.Helper()
	hash, err := Hash(directories...)
	if err != nil {
		t.Fatal(err)
	}
	recorded, latest, err := read(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if existing, ok := recorded[version]; ok {
		if existing != hash {
			t.Fatalf("runtime sources changed after Version %d was released.\n"+
				"Raise Version, then run: go test ./internal/runtime/scheduler ./internal/runtime/supervisor -run SourceLock -args -update-source-lock", version)
		}
		return
	}
	if version <= latest {
		t.Fatalf("Version %d must be greater than the last recorded version %d", version, latest)
	}
	if !*update {
		t.Fatalf("Version %d is not recorded in %s.\nRun: go test ./internal/runtime/scheduler ./internal/runtime/supervisor -run SourceLock -args -update-source-lock", version, lockPath)
	}
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := fmt.Fprintf(file, "%d %s\n", version, hash); err != nil {
		t.Fatal(err)
	}
}

func read(path string) (map[int]string, int, error) {
	recorded, latest := map[int]string{}, 0
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return recorded, latest, nil
	}
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		version, err := strconv.Atoi(fields[0])
		if len(fields) != 2 || err != nil {
			return nil, 0, fmt.Errorf("%s: malformed line %q", path, line)
		}
		recorded[version] = fields[1]
		if version > latest {
			latest = version
		}
	}
	return recorded, latest, scanner.Err()
}
