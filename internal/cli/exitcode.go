package cli

import (
	"context"
	"errors"

	"github.com/xavidop/tobira/driver"
)

// Process exit codes.
const (
	exitOK         = 0
	exitUsage      = 1
	exitConnection = 2
	exitQuery      = 3
	exitDenied     = 4
)

// exitError carries an explicit exit code for errors that are not
// classified by a driver sentinel (usage and configuration problems).
type exitError struct {
	code int
	err  error
	// silent means the command already said everything: Execute prints no
	// "error:" line for it (`tobira doctor` exiting 1 or 2 on its report).
	silent bool
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

// usageError marks err as a command-line usage error (exit code 1).
func usageError(err error) error { return &exitError{code: exitUsage, err: err} }

// ExitCode maps err to a process exit code: 0 for nil, 1 for usage errors,
// 2 when the connection could not be opened (any failure while resolving or
// opening it, plus connection, authentication and unknown-driver errors), 4
// for read-only and permission errors, and 3 for everything else (including
// cancellation and timeouts).
func ExitCode(err error) int {
	if err == nil {
		return exitOK
	}
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.code
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return exitQuery
	}
	switch driver.KindOf(err) {
	case driver.ErrConnection, driver.ErrAuth, driver.ErrUnknownDriver:
		return exitConnection
	case driver.ErrReadOnly, driver.ErrPermission:
		return exitDenied
	}
	return exitQuery
}
