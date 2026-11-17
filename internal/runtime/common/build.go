package common

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"runtime"
)

// BuildID identifies the running runtime build: the first 12 hex digits of
// the SHA-256 of its executable. Installers compare whole files the same way
// and replace a runtime, and move its processes, only when the file changed.
// Release builds are reproducible, so an unchanged runtime keeps its ID.
func BuildID() string {
	path, err := os.Executable()
	if err != nil {
		return "unknown"
	}
	id, err := FileBuildID(path)
	if err != nil {
		return "unknown"
	}
	return id
}

// FileBuildID is the BuildID a runtime binary at path reports once it runs.
func FileBuildID(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil))[:12], nil
}

// VersionLine is what "<runtime> version" prints.
func VersionLine(name string) string {
	return name + " " + BuildID() + " (" + runtime.Version() + ")"
}
