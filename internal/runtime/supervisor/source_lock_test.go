package supervisor

import (
	"testing"

	"github.com/eyvant/spm/internal/runtime/sourcelock"
)

func TestSourceLock(t *testing.T) {
	sourcelock.Check(t, "source.lock", Version, ".", "../common")
}
