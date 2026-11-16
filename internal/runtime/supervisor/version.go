package supervisor

// Version is the spm-supervisor runtime version. Installers replace
// spm-supervisor and restart supervised services only when it changes, so it
// must be raised with every change to this package or internal/runtime/common;
// TestSourceLock enforces that.
const Version = 2
