package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"strings"

	anthropicsdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/firebase/genkit/go/core/status"
	openaisdk "github.com/openai/openai-go"
	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/internal/assistant"
)

// providerError is a provider or init failure with its message redacted.
// It unwraps to the assistant sentinel (and to a context error, for a
// cancel), never to the original error, whose text may hold the key.
type providerError struct {
	msg  string
	kind []error
}

func (e *providerError) Error() string   { return e.msg }
func (e *providerError) Unwrap() []error { return e.kind }

// wrapProviderError classifies err as an assistant error and redacts its
// message against key. nil stays nil.
func wrapProviderError(err error, key driver.Secret) error {
	if err == nil {
		return nil
	}
	kind := classify(err)
	kinds := []error{kind}
	for _, c := range []error{context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, c) {
			kinds = append(kinds, c)
		}
	}
	return &providerError{msg: kind.Error() + ": " + redact(err.Error(), key), kind: kinds}
}

// classify maps err to an assistant sentinel.
func classify(err error) error {
	if errors.Is(err, context.Canceled) {
		return assistant.ErrProvider
	}
	code := status.Name("")
	if n, ok := status.Classified(err); ok {
		code = n
	}
	var oe *openaisdk.Error
	var ae *anthropicsdk.Error
	switch {
	case errors.As(err, &oe):
		code = status.FromHTTPCode(oe.StatusCode)
	case errors.As(err, &ae):
		code = status.FromHTTPCode(ae.StatusCode)
	}
	switch code {
	case status.Unauthenticated, status.PermissionDenied:
		return assistant.ErrAuth
	case status.ResourceExhausted:
		return assistant.ErrRateLimit
	case status.Unavailable, status.DeadlineExceeded:
		return assistant.ErrUnavailable
	}
	var ne net.Error
	var ue *url.Error
	var op *net.OpError
	if errors.As(err, &ne) || errors.As(err, &ue) || errors.As(err, &op) {
		return assistant.ErrUnavailable
	}
	return assistant.ErrProvider
}

// redact masks key and every credential the driver redactor recognises in
// msg. The key is masked whatever its length, raw, URL-escaped and
// JSON-escaped (with and without HTML escaping, and with "/" as "\/"),
// since provider errors often quote the request URL or body. A provider's
// own partial mask (OpenAI's "sk-...abcd") cannot be matched and is left: it
// shows at most the last few characters, which the provider already chose
// to reveal.
func redact(msg string, key driver.Secret) string {
	if k := key.Reveal(); k != "" {
		forms := []string{k, url.QueryEscape(k), url.PathEscape(k)}
		for _, j := range jsonForms(k) {
			forms = append(forms, j, strings.ReplaceAll(j, "/", `\/`))
		}
		for _, form := range forms {
			msg = strings.ReplaceAll(msg, form, "xxxxx")
		}
	}
	return driver.RedactMessage(msg, driver.ConnConfig{Password: key})
}

// jsonForms returns k as the inside of a JSON string, HTML-escaped (Go's
// default: \u0026 for &) and not.
func jsonForms(k string) []string {
	var out []string
	for _, html := range []bool{true, false} {
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(html)
		if enc.Encode(k) == nil {
			out = append(out, strings.TrimSuffix(strings.TrimSpace(b.String()), `"`)[1:])
		}
	}
	return out
}
