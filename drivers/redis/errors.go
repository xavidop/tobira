package redis

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"

	goredis "github.com/redis/go-redis/v9"

	"github.com/xavidop/tobira/driver"
)

// mapError classifies err as a *driver.Error. Context errors keep their
// identity and carry no kind. A server error reply is classified by its
// error code (the first word when it is upper case, as in "WRONGTYPE ..."),
// which is also stored in Error.Code. A lost connection is ErrConnection
// unless ctx is done, in which case our own cancellation caused it and the
// ctx error is joined instead.
func mapError(ctx context.Context, op string, err error) error {
	if err == nil {
		return nil
	}
	var de *driver.Error
	if errors.As(err, &de) {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return driver.Wrap(nil, Name, op, err)
	}
	var ctxErr error
	if ctx != nil {
		ctxErr = ctx.Err()
	}
	var re goredis.Error
	if errors.As(err, &re) && !errors.Is(err, goredis.ErrClosed) {
		code := errorCode(err.Error())
		return &driver.Error{Kind: kindForReply(code, err.Error()), Driver: Name, Op: op, Code: code, Err: err}
	}
	var ne net.Error
	if errors.Is(err, goredis.ErrClosed) || errors.As(err, &ne) || errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
		if ctxErr != nil {
			return &driver.Error{Driver: Name, Op: op, Err: errors.Join(err, ctxErr)}
		}
		return driver.Wrap(driver.ErrConnection, Name, op, err)
	}
	var oe *net.OpError
	if errors.As(err, &oe) {
		return driver.Wrap(driver.ErrConnection, Name, op, err)
	}
	return driver.Wrap(nil, Name, op, err)
}

// errorCode returns the error code of a Redis error reply: its first word
// when that word is upper case letters, digits and underscores ("ERR",
// "WRONGTYPE", "SEARCH_INDEX_NOT_FOUND"), else "".
func errorCode(msg string) string {
	word, _, _ := strings.Cut(msg, " ")
	if word == "" {
		return ""
	}
	for i := 0; i < len(word); i++ {
		b := word[i]
		if (b < 'A' || b > 'Z') && (b < '0' || b > '9') && b != '_' && b != '-' {
			return ""
		}
	}
	return word
}

// kindForReply maps a Redis error reply to a driver error kind, or nil.
func kindForReply(code, msg string) error {
	switch code {
	case "WRONGPASS", "NOAUTH":
		return driver.ErrAuth
	case "NOPERM":
		return driver.ErrPermission
	case "WRONGTYPE":
		return driver.ErrConflict
	case "READONLY":
		return driver.ErrReadOnly
	case "LOADING", "BUSY", "MASTERDOWN", "CLUSTERDOWN", "TRYAGAIN":
		return driver.ErrConnection
	case "MOVED", "ASK", "CROSSSLOT":
		return driver.ErrUnsupported
	case "SEARCH_INDEX_NOT_FOUND":
		return driver.ErrNotFound
	}
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "unknown index name"), strings.Contains(lower, "no such index"),
		strings.Contains(lower, "index not found"), strings.HasSuffix(lower, "no such key"):
		return driver.ErrNotFound
	case strings.Contains(lower, "unknown command"), strings.Contains(lower, "unknown subcommand"),
		strings.Contains(lower, "syntax error"), strings.Contains(lower, "wrong number of arguments"):
		return driver.ErrSyntax
	case strings.Contains(lower, "invalid password"), strings.Contains(lower, "without any password configured"):
		return driver.ErrAuth
	}
	return nil
}
