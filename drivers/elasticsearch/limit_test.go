package elasticsearch

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/xavidop/tobira/driver"
)

// A response over the cap is a size limit (ErrLimit).
func TestResponseOverCapIsErrLimit(t *testing.T) {
	c := newFakeConn(t, &fakeES{n: 200, product: true}, flavorElastic, false)
	old := maxResponse
	maxResponse = 1024
	t.Cleanup(func() { maxResponse = old })
	_, err := c.do(context.Background(), "query", http.MethodPost, pathOf("docs", "_search"), nil, []byte(`{"size":200}`))
	if !errors.Is(err, driver.ErrLimit) {
		t.Errorf("response over the cap = %v, want ErrLimit", err)
	}
}
