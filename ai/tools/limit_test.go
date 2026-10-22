package tools

import (
	"errors"
	"strings"
	"testing"

	"github.com/xavidop/tobira/driver"
)

// A size-limit error reaches the model as a fixed text that says how to
// get under the limit, never the driver's message.
func TestScrubSizeLimit(t *testing.T) {
	err := driver.Wrap(driver.ErrLimit, "libsql", "query", errors.New("row 'secret' is too wide"))
	got := scrubDriverError(err, func(s string) string { return s })
	if !strings.HasPrefix(got, "size limit exceeded: ") || !strings.Contains(got, "LIMIT") || strings.Contains(got, "secret") {
		t.Errorf("scrubDriverError = %q", got)
	}
}
