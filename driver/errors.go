package driver

import (
	"errors"
	"strings"
)

// Sentinel errors classify every failure a driver can report. They are a
// closed set: a *Error crossing the plugin boundary travels as a kind name
// (see KindName and KindByName) and is rebuilt on the host.
var (
	// ErrNotFound means a namespace, collection, row or key is missing.
	ErrNotFound = errors.New("not found")
	// ErrAuth means the credentials were rejected.
	ErrAuth = errors.New("authentication failed")
	// ErrPermission means the caller is authenticated but not allowed.
	ErrPermission = errors.New("permission denied")
	// ErrReadOnly means a read-only guard blocked a mutation.
	ErrReadOnly = errors.New("read-only violation")
	// ErrUnsupported means the driver does not implement the operation.
	ErrUnsupported = errors.New("not supported")
	// ErrConnection means the database is unreachable: network, TLS, DNS or a closed connection.
	ErrConnection = errors.New("connection failed")
	// ErrSyntax means the query text is invalid.
	ErrSyntax = errors.New("invalid query")
	// ErrConstraint means a constraint was violated.
	ErrConstraint = errors.New("constraint violation")
	// ErrConflict means an ambiguous key or a serialization failure.
	ErrConflict = errors.New("conflict")
	// ErrLimit means a size limit was hit: a result over the driver's
	// memory budget, a row over the plugin protocol's message cap, or a
	// query over a cost cap such as BigQuery's max_bytes_billed. Narrowing
	// the query (a LIMIT, fewer columns) or raising the limit gets past it.
	ErrLimit = errors.New("size limit exceeded")
	// ErrUnknownDriver means no driver is registered under the requested name.
	ErrUnknownDriver = errors.New("unknown driver")
)

// kinds maps each sentinel to its stable snake_case name.
var kinds = []struct {
	name string
	err  error
}{
	{"not_found", ErrNotFound},
	{"auth", ErrAuth},
	{"permission", ErrPermission},
	{"read_only", ErrReadOnly},
	{"unsupported", ErrUnsupported},
	{"connection", ErrConnection},
	{"syntax", ErrSyntax},
	{"constraint", ErrConstraint},
	{"conflict", ErrConflict},
	{"limit", ErrLimit},
	{"unknown_driver", ErrUnknownDriver},
}

// Error carries classification plus native detail. errors.Is(err, ErrX) works
// through Kind; errors.Is(err, context.Canceled/DeadlineExceeded) works through Err.
type Error struct {
	// Kind is one of the sentinels above, or nil if unclassified.
	Kind error
	// Driver is the name of the driver that produced the error.
	Driver string
	// Op is the operation that failed: "open", "query", "describe", "browse" and so on.
	Op string
	// Code is the native error code, "" when there is none: the SQLSTATE
	// for PostgreSQL ("42P01"), the error number for MySQL ("1366"), the
	// extended result code for SQLite ("2067"). Hosts may rely on it: the
	// AI tools recognise data exceptions by it (SQLSTATE classes 22 and 23,
	// MySQL value errors) and then drop the message, which can quote row
	// values. A driver should fill it whenever the database gives one.
	Code string
	// Err is the underlying error.
	Err error
}

// Error renders "driver: op: message (code): kind", omitting any empty part.
// For example: postgres: query: relation "x" does not exist (42P01): not found.
func (e *Error) Error() string {
	msg := ""
	if e.Err != nil {
		msg = e.Err.Error()
	}
	if e.Code != "" {
		if msg != "" {
			msg += " "
		}
		msg += "(" + e.Code + ")"
	}
	parts := make([]string, 0, 4)
	for _, p := range []string{e.Driver, e.Op, msg} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if e.Kind != nil {
		parts = append(parts, e.Kind.Error())
	}
	return strings.Join(parts, ": ")
}

// Unwrap returns the non-nil Kind and Err, so errors.Is and errors.As see both.
func (e *Error) Unwrap() []error {
	errs := make([]error, 0, 2)
	if e.Kind != nil {
		errs = append(errs, e.Kind)
	}
	if e.Err != nil {
		errs = append(errs, e.Err)
	}
	return errs
}

// Wrap classifies err as kind and attributes it to a driver and operation.
// It returns nil when err is nil.
func Wrap(kind error, driver, op string, err error) error {
	if err == nil {
		return nil
	}
	return &Error{Kind: kind, Driver: driver, Op: op, Err: err}
}

// KindOf returns the sentinel that classifies err, or nil if err is nil or
// unclassified.
func KindOf(err error) error {
	if err == nil {
		return nil
	}
	for _, k := range kinds {
		if errors.Is(err, k.err) {
			return k.err
		}
	}
	return nil
}

// KindName returns the snake_case name of err's kind ("not_found", "auth",
// "read_only" and so on), or "" if err is nil or unclassified.
func KindName(err error) string {
	k := KindOf(err)
	for _, e := range kinds {
		if e.err == k && k != nil {
			return e.name
		}
	}
	return ""
}

// KindByName is the inverse of KindName. It returns nil for an unknown name.
func KindByName(name string) error {
	for _, k := range kinds {
		if k.name == name {
			return k.err
		}
	}
	return nil
}
