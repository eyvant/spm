package scheduler

// Version is the spm-scheduler runtime version. Installers replace
// spm-scheduler and restart the scheduler daemon only when it changes, so it
// must be raised with every change to this package or internal/runtime/common;
// TestSourceLock enforces that.
const Version = 1
