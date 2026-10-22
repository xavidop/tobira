package sdk_test

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/xavidop/tobira/driver"
	pb "github.com/xavidop/tobira/plugin/proto/tobira/plugin/v1"
	"github.com/xavidop/tobira/plugin/sdk"
)

// The password every panic in this file quotes. The DSN carries it, and so
// does ConnConfig.Password.
const panicSecret = "hunter22"

var panicConfig = &pb.ConnConfig{Dsn: "fake://u:" + panicSecret + "@db/x", Password: panicSecret}

// openPanicDriver panics in Open, quoting the config's secrets.
type openPanicDriver struct{ driver.Driver }

func (openPanicDriver) Open(_ context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	panic("open exploded on " + cfg.DSN + " with " + cfg.Password.Reveal())
}

// lifecyclePanicDriver's Conns and Txs panic in Close, Rollback or (after
// a gate) Begin's rollback, quoting the DSN.
type lifecyclePanicDriver struct {
	driver.Driver
	closePanics, rollbackPanics bool
	// beginGate, when set, makes Begin wait for it after starting the Tx.
	beginGate chan struct{}
	beganTx   chan struct{}
}

type lifecyclePanicConn struct {
	driver.Conn
	d   *lifecyclePanicDriver
	dsn string
}

type lifecyclePanicTx struct {
	driver.Tx
	c lifecyclePanicConn
}

func (d *lifecyclePanicDriver) Open(ctx context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	c, err := d.Driver.Open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return lifecyclePanicConn{Conn: c, d: d, dsn: cfg.DSN}, nil
}

func (c lifecyclePanicConn) Close() error {
	if c.d.closePanics {
		panic("close exploded on " + c.dsn)
	}
	return c.Conn.Close()
}

func (c lifecyclePanicConn) Begin(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	tx, err := c.Conn.(driver.Transactor).Begin(ctx, opts)
	if err != nil {
		return nil, err
	}
	if c.d.beginGate != nil {
		c.d.beganTx <- struct{}{}
		<-c.d.beginGate
	}
	return lifecyclePanicTx{Tx: tx, c: c}, nil
}

func (t lifecyclePanicTx) Rollback(ctx context.Context) error {
	if t.c.d.rollbackPanics {
		panic("rollback exploded on " + t.c.dsn)
	}
	return t.Tx.Rollback(ctx)
}

// wantRedactedPanic asserts the call failed with the generic panic status
// and that the log has the panic, but not the secret.
func wantRedactedPanic(t *testing.T, what string, err error, logs *syncBuffer, panicText string) {
	t.Helper()
	wantStatus(t, what, err, codes.Internal, "")
	if msg := status.Convert(err).Message(); msg != "plugin panic" {
		t.Fatalf("%s: host saw %q, want the generic message", what, msg)
	}
	out := logs.String()
	if !strings.Contains(out, panicText) {
		t.Fatalf("%s: stderr log lacks %q:\n%s", what, panicText, out)
	}
	if strings.Contains(out, panicSecret) {
		t.Fatalf("%s: panic log leaked the password:\n%s", what, out)
	}
}

func panicLogger(logs *syncBuffer) sdk.Option {
	return sdk.WithLogger(slog.New(slog.NewJSONHandler(logs, nil)))
}

func TestOpenPanicIsRedacted(t *testing.T) {
	var logs syncBuffer
	c, _ := startWith(t, openPanicDriver{newFake().Driver()}, panicLogger(&logs))
	_, err := c.Open(context.Background(), &pb.OpenRequest{Config: panicConfig})
	wantRedactedPanic(t, "Open", err, &logs, "open exploded")
}

func TestClosePanicIsRedacted(t *testing.T) {
	var logs syncBuffer
	d := &lifecyclePanicDriver{Driver: newFake().Driver(), closePanics: true}
	c, _ := startWith(t, d, panicLogger(&logs))
	h := open(t, c, panicConfig).GetConn()
	_, err := c.Close(context.Background(), &pb.CloseRequest{Conn: h})
	wantRedactedPanic(t, "Close", err, &logs, "close exploded")
}

func TestCloseRollbackPanicIsRedacted(t *testing.T) {
	var logs syncBuffer
	d := &lifecyclePanicDriver{Driver: newFake().Driver(), rollbackPanics: true}
	c, _ := startWith(t, d, panicLogger(&logs))
	ctx := context.Background()
	h := open(t, c, panicConfig).GetConn()
	if _, err := c.Begin(ctx, &pb.BeginRequest{Conn: h}); err != nil {
		t.Fatal(err)
	}
	_, err := c.Close(ctx, &pb.CloseRequest{Conn: h})
	wantRedactedPanic(t, "Close with an open Tx", err, &logs, "rollback exploded")
}

func TestBeginLostRaceRollbackPanicIsRedacted(t *testing.T) {
	var logs syncBuffer
	d := &lifecyclePanicDriver{
		Driver: newFake().Driver(), rollbackPanics: true,
		beginGate: make(chan struct{}), beganTx: make(chan struct{}),
	}
	c, _ := startWith(t, d, panicLogger(&logs))
	ctx := context.Background()
	h := open(t, c, panicConfig).GetConn()
	done := make(chan error, 1)
	go func() {
		_, err := c.Begin(ctx, &pb.BeginRequest{Conn: h})
		done <- err
	}()
	<-d.beganTx
	// Close the Conn while Begin runs: Begin then rolls its Tx back.
	if _, err := c.Close(ctx, &pb.CloseRequest{Conn: h}); err != nil {
		t.Fatalf("Close: %v", err)
	}
	close(d.beginGate)
	wantRedactedPanic(t, "Begin that lost the race", <-done, &logs, "rollback exploded")
}

// panicDriver's Conns panic in Ping, quoting their DSN, when the DSN asks.
type panicDriver struct{ driver.Driver }

type panicConn struct {
	driver.Conn
	dsn string
}

func (c panicConn) Ping(ctx context.Context) error {
	if strings.Contains(c.dsn, "panic") {
		panic("ping exploded on " + c.dsn)
	}
	return c.Conn.Ping(ctx)
}

func (d panicDriver) Open(ctx context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	c, err := d.Driver.Open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return panicConn{Conn: c, dsn: cfg.DSN}, nil
}

func TestPanicsAreRecovered(t *testing.T) {
	f := newFake()
	f.OnQuery("boom", func(driver.Query) (driver.ResultSet, error) { panic("query exploded") })
	var logs syncBuffer
	c, _ := startWith(t, panicDriver{f.Driver()}, sdk.WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))))
	ctx := context.Background()
	const secret = "hunter22"
	bad := open(t, c, &pb.ConnConfig{Dsn: "fake://u:" + secret + "@panic/db"}).GetConn()
	good := open(t, c, &pb.ConnConfig{}).GetConn()

	_, err := c.Ping(ctx, &pb.PingRequest{Conn: bad})
	wantStatus(t, "unary panic", err, codes.Internal, "")
	if msg := status.Convert(err).Message(); msg != "plugin panic" {
		t.Fatalf("host saw %q, want the generic message", msg)
	}

	s, err := c.Query(ctx, &pb.QueryRequest{Conn: good, Query: &pb.Query{Text: "boom"}})
	if err == nil {
		_, err = s.Recv()
	}
	wantStatus(t, "stream panic", err, codes.Internal, "")

	// The plugin keeps serving, the panicking Conn included.
	if _, err := c.Ping(ctx, &pb.PingRequest{Conn: good}); err != nil {
		t.Fatalf("after the panics: %v", err)
	}
	if rows := queryRows(t, c, good, "select * from tobira_people where id = 1"); len(rows) != 1 {
		t.Fatalf("query after the panics: %v", rows)
	}

	out := logs.String()
	for _, want := range []string{"ping exploded", "query exploded", "goroutine", "/Ping"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stderr log lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, secret) {
		t.Fatalf("panic log leaked the DSN password:\n%s", out)
	}
}
