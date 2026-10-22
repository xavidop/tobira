package query_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
	"github.com/xavidop/tobira/internal/query"
)

func openFake(t *testing.T, opts ...drivertest.FakeOption) (*drivertest.Fake, driver.Conn) {
	t.Helper()
	f := drivertest.NewFake(opts...)
	c, err := f.Driver().Open(context.Background(), driver.ConnConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return f, c
}

func TestRunLimitTruncates(t *testing.T) {
	_, c := openFake(t, drivertest.WithPeople())
	res, err := query.Run(context.Background(), c, "select * from tobira_people", query.Options{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 10 || !res.Stats.Truncated {
		t.Fatalf("rows=%d truncated=%v, want 10 true", len(res.Rows), res.Stats.Truncated)
	}
	if len(res.Columns) == 0 {
		t.Fatal("no columns")
	}
}

func TestRunNoLimit(t *testing.T) {
	_, c := openFake(t, drivertest.WithPeople())
	res, err := query.Run(context.Background(), c, "select * from tobira_people", query.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stats.Truncated || len(res.Rows) <= 10 {
		t.Fatalf("rows=%d truncated=%v", len(res.Rows), res.Stats.Truncated)
	}
}

func TestRunTimeout(t *testing.T) {
	f, c := openFake(t, drivertest.WithPeople())
	f.SetLatency(5 * time.Second)
	start := time.Now()
	_, err := query.Run(context.Background(), c, "select * from tobira_people", query.Options{Timeout: 100 * time.Millisecond})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("took %v", d)
	}
}

func TestRunReadOnly(t *testing.T) {
	_, c := openFake(t, drivertest.WithPeople())
	_, err := query.Run(context.Background(), c, "delete from tobira_people", query.Options{ReadOnly: true})
	if !errors.Is(err, driver.ErrReadOnly) {
		t.Fatalf("err = %v, want ErrReadOnly", err)
	}
}

func TestRunWriteReportsRowsAffected(t *testing.T) {
	_, c := openFake(t, drivertest.WithPeople())
	res, err := query.Run(context.Background(), c, "delete from tobira_people where id = 1", query.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Columns) != 0 || res.Stats.RowsAffected != 1 {
		t.Fatalf("cols=%d affected=%d", len(res.Columns), res.Stats.RowsAffected)
	}
}

func TestRunExplain(t *testing.T) {
	_, c := openFake(t, drivertest.WithPeople())
	res, err := query.Run(context.Background(), c, "select * from tobira_people", query.Options{Explain: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Plan == nil || res.Plan.Text == "" || len(res.Rows) != 0 {
		t.Fatalf("plan=%v rows=%d", res.Plan, len(res.Rows))
	}
}

func TestRunExplainUnsupported(t *testing.T) {
	_, c := openFake(t, drivertest.WithPeople(), drivertest.WithoutCapabilities(driver.CapExplainer))
	_, err := query.Run(context.Background(), c, "select * from tobira_people", query.Options{Explain: true, Driver: "fake"})
	if !errors.Is(err, driver.ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
	var de *driver.Error
	if !errors.As(err, &de) || de.Driver != "fake" {
		t.Fatalf("want *driver.Error with Driver fake, got %#v", err)
	}
}
