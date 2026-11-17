// Package supervisor implements spm-supervisor, the process that runs every
// Windows and macOS service managed by spm. It starts the service command,
// copies its output into service.log with rotation, and records what it does
// in spm.log. On Windows it also applies the restart policy; on macOS launchd
// does, and the supervisor mirrors the service's exit status. Linux needs no
// supervisor: systemd runs services and journald keeps their output.
//
// spm-supervisor stays running for as long as its service runs, so this
// package changes rarely. Installers replace the binary only when the file
// changes (see common.BuildID) and then move running services onto it without
// restarting them.
package supervisor
