package redis

import (
	"errors"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

// A reply over the size bounds is a size limit (ErrLimit), with the hint.
func TestReplyTooLargeIsErrLimit(t *testing.T) {
	err := replyTooLarge([]string{"hgetall", "h"}, "HLEN", maxReplyElements+1)
	if !errors.Is(err, driver.ErrLimit) || !strings.Contains(err.Error(), tooLargeHint) {
		t.Errorf("replyTooLarge = %v, want ErrLimit with the hint", err)
	}
	if err := replyTooLarge([]string{"get", "k"}, "STRLEN", maxReplyBytes+1); !strings.Contains(err.Error(), "bytes") {
		t.Errorf("STRLEN reply: %v, want a byte count", err)
	}
}
