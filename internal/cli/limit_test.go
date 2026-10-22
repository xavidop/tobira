package cli_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/cli"
)

// A size-limit error is printed with a hint on how to get under the limit,
// still on the single error line, and exits as a query failure.
func TestExecuteSizeLimitMessage(t *testing.T) {
	var errOut bytes.Buffer
	root := &cobra.Command{
		Use: "x", SilenceErrors: true, SilenceUsage: true,
		RunE: func(*cobra.Command, []string) error {
			return driver.Wrap(driver.ErrLimit, "libsql", "query", errors.New("the result exceeds the memory budget"))
		},
	}
	root.SetErr(&errOut)
	code := cli.Execute(context.Background(), root, []string{})
	got := errOut.String()
	if code != 3 {
		t.Errorf("exit code %d, want 3", code)
	}
	if !strings.HasPrefix(got, "error: libsql: query: the result exceeds the memory budget: size limit exceeded") ||
		!strings.Contains(got, "narrow the query") || strings.Count(got, "\n") != 1 {
		t.Errorf("stderr %q, want the error with a size-limit hint on one line", got)
	}
}
