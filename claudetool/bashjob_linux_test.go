package claudetool

import (
	"errors"
	"testing"

	"golang.org/x/sys/unix"
)

// A failed pidfd_open is an error, not a vanished job: recovery must not
// report a live job lost.
func TestBackgroundJobPidfdError(t *testing.T) {
	job := BackgroundJob{PID: -1}
	if _, err := job.Exited(); !errors.Is(err, unix.EINVAL) {
		t.Errorf("Exited: %v, want EINVAL", err)
	}
	if err := job.Kill(); !errors.Is(err, unix.EINVAL) {
		t.Errorf("Kill: %v, want EINVAL", err)
	}
}
