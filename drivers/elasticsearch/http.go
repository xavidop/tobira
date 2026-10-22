package elasticsearch

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/xavidop/tobira/driver"
)

// maxResponse caps the bytes read from one response, so a huge result
// fails cleanly (ErrLimit) instead of exhausting memory. A variable so
// tests can lower it.
var maxResponse = 64 << 20

// reqPath is a request path: the decoded segments as the server reads
// them, and the escaped form sent on the wire.
type reqPath struct{ plain, escaped string }

// pathOf joins segments into a path, escaping each one, so an index name
// or document id can never add a path segment.
func pathOf(segs ...string) reqPath {
	var p, e strings.Builder
	for _, s := range segs {
		p.WriteString("/" + s)
		e.WriteString("/" + url.PathEscape(s))
	}
	if p.Len() == 0 {
		return reqPath{"/", "/"}
	}
	return reqPath{p.String(), e.String()}
}

// rawPath is a path the guard validated: it holds no character that
// needs escaping, so it is sent as is.
func rawPath(p string) reqPath { return reqPath{p, p} }

// do sends one request and returns the response body of a 2xx answer; any
// other status is a classified *driver.Error. body may be nil.
func (c *conn) do(ctx context.Context, op, method string, path reqPath, params url.Values, body []byte) ([]byte, error) {
	return c.doType(ctx, op, method, path, params, body, "application/json")
}

func (c *conn) doType(ctx context.Context, op, method string, path reqPath, params url.Values, body []byte, contentType string) ([]byte, error) {
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	return c.send(ctx, op, method, path, params, body, contentType)
}

// send is doType without the closed-connection check, for releasing
// points in time while Close runs.
func (c *conn) send(ctx context.Context, op, method string, path reqPath, params url.Values, body []byte, contentType string) ([]byte, error) {
	u := &url.URL{Path: path.plain, RawPath: path.escaped, RawQuery: params.Encode()}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return nil, mapError(ctx, c.name(), op, err)
	}
	// NewRequest re-parses the URL; keep the exact escaping chosen above.
	req.URL = u
	if body != nil {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", "application/json")
	res, err := c.client.Perform(req)
	if err != nil {
		return nil, mapError(ctx, c.name(), op, err)
	}
	defer func() { _ = res.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(res.Body, int64(maxResponse)+1))
	if err != nil {
		return nil, mapError(ctx, c.name(), op, err)
	}
	if len(data) > maxResponse {
		return nil, driver.Wrap(driver.ErrLimit, c.name(), op, fmt.Errorf("the response is larger than %d MiB: narrow the query or lower its size", maxResponse>>20))
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return nil, httpError(c.name(), op, res.StatusCode, data)
	}
	return data, nil
}
