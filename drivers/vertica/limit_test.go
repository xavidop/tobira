package vertica

import (
	"errors"
	"testing"

	"github.com/xavidop/tobira/driver"
)

// Vertica's resource SQLSTATEs (class 53, insufficient resources such as
// out of memory, and 54, a program limit) are size limits.
func TestResourceStatesAreErrLimit(t *testing.T) {
	for _, st := range []string{"53000", "53200", "54000", "54001"} {
		if k := kindForState(st); !errors.Is(k, driver.ErrLimit) {
			t.Errorf("kindForState(%s) = %v, want ErrLimit", st, k)
		}
	}
	if k := kindForState("22001"); k != nil {
		t.Errorf("kindForState(22001) = %v, want none (a data exception)", k)
	}
}
