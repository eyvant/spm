package common

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

const (
	DefaultLogMaxBytes = int64(10 * 1024 * 1024)
	DefaultLogBackups  = 5
)

type RotatingLog struct {
	mu      sync.Mutex
	path    string
	file    *os.File
	size    int64
	maximum int64
	backups int
}

func OpenRotatingLog(path string, maximum int64, backups int) (*RotatingLog, error) {
	if maximum <= 0 || backups < 0 {
		return nil, fmt.Errorf("invalid log rotation limits")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	log := &RotatingLog{path: path, maximum: maximum, backups: backups}
	if info, err := os.Stat(path); err == nil && info.Size() >= maximum {
		if err := rotateLogFiles(path, backups); err != nil {
			return nil, err
		}
	} else if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if err := log.open(); err != nil {
		return nil, err
	}
	return log, nil
}

func (log *RotatingLog) open() error {
	file, err := os.OpenFile(log.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return err
	}
	log.file, log.size = file, info.Size()
	return nil
}

func (log *RotatingLog) Write(data []byte) (int, error) {
	log.mu.Lock()
	defer log.mu.Unlock()
	written := 0
	for len(data) > 0 {
		if log.size >= log.maximum {
			if err := log.rotate(); err != nil {
				return written, err
			}
		}
		limit := int(log.maximum - log.size)
		if limit > len(data) {
			limit = len(data)
		}
		count, err := log.file.Write(data[:limit])
		written += count
		log.size += int64(count)
		data = data[count:]
		if err != nil {
			return written, err
		}
		if count == 0 {
			return written, os.ErrInvalid
		}
	}
	return written, nil
}

func (log *RotatingLog) rotate() error {
	if err := log.file.Close(); err != nil {
		return err
	}
	log.file = nil
	if err := rotateLogFiles(log.path, log.backups); err != nil {
		return err
	}
	return log.open()
}

func (log *RotatingLog) Close() error {
	log.mu.Lock()
	defer log.mu.Unlock()
	if log.file == nil {
		return nil
	}
	err := log.file.Close()
	log.file = nil
	return err
}

func rotateLogFiles(path string, backups int) error {
	if backups == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	oldest := fmt.Sprintf("%s.%d", path, backups)
	if err := os.Remove(oldest); err != nil && !os.IsNotExist(err) {
		return err
	}
	for index := backups - 1; index >= 1; index-- {
		from := fmt.Sprintf("%s.%d", path, index)
		to := fmt.Sprintf("%s.%d", path, index+1)
		if err := os.Rename(from, to); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := os.Rename(path, path+".1"); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// CompileLogGrep compiles a --grep pattern the way journalctl reads one, so
// spm logs filters alike on every platform: a regular expression that
// ignores case when the pattern has no uppercase letters. An empty pattern
// returns nil, which matches every line.
func CompileLogGrep(pattern string) (*regexp.Regexp, error) {
	if pattern == "" {
		return nil, nil
	}
	expression := pattern
	if strings.ToLower(pattern) == pattern {
		expression = "(?i)" + pattern
	}
	compiled, err := regexp.Compile(expression)
	if err != nil {
		return nil, fmt.Errorf("invalid --grep pattern %q: %w", pattern, err)
	}
	return compiled, nil
}

func PrintRotatedLogTail(w io.Writer, path string, count int, grep *regexp.Regexp) error {
	var data []byte
	for index := DefaultLogBackups; index >= 0; index-- {
		candidate := path
		if index > 0 {
			candidate = fmt.Sprintf("%s.%d", path, index)
		}
		part, err := os.ReadFile(candidate)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		data = append(data, part...)
	}
	var lines []string
	for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		if line != "" && (grep == nil || grep.MatchString(line)) {
			lines = append(lines, line)
		}
	}
	if count >= 0 && len(lines) > count {
		lines = lines[len(lines)-count:]
	}
	for _, line := range lines {
		fmt.Fprintln(w, line)
	}
	return nil
}

func ReadRotatingLogUpdates(w io.Writer, path string, previous os.FileInfo, offset int64, grep *regexp.Regexp) (os.FileInfo, int64, error) {
	current, err := os.Stat(path)
	if os.IsNotExist(err) {
		return previous, offset, nil
	}
	if err != nil {
		return previous, offset, err
	}
	rotatedBackup := ""
	if previous != nil {
		for index := 1; index <= DefaultLogBackups; index++ {
			backup := fmt.Sprintf("%s.%d", path, index)
			info, statErr := os.Stat(backup)
			if statErr == nil && sameLogFile(previous, info) {
				rotatedBackup = backup
				break
			}
		}
	}
	if rotatedBackup != "" {
		if _, readErr := printLogFromOffset(w, rotatedBackup, offset, grep); readErr != nil {
			return previous, offset, readErr
		}
		offset = 0
	} else if previous != nil && !sameLogFile(previous, current) {
		offset = 0
	} else if current.Size() < offset {
		offset = 0
	}
	next, err := printLogFromOffset(w, path, offset, grep)
	return current, next, err
}

func sameLogFile(left, right os.FileInfo) bool {
	return os.SameFile(left, right) || left.Size() == right.Size() && left.ModTime().Equal(right.ModTime())
}

func printLogFromOffset(w io.Writer, path string, offset int64, grep *regexp.Regexp) (int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return offset, err
	}
	defer file.Close()
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return offset, err
	}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if grep == nil || grep.MatchString(line) {
			fmt.Fprintln(w, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return offset, err
	}
	return file.Seek(0, io.SeekCurrent)
}
