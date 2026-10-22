//go:build integration

package mysql

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/xavidop/tobira/driver"
)

// hugeQuery returns 200,000 rows of about 16 KB each (about 3 GB), far more
// than can be drained in the time the tests allow.
const hugeQuery = "SELECT a.n, b.n AS m, REPEAT('x', 16000) AS pad FROM tobira_big a JOIN tobira_big b ON b.n <= 200"

func within(t *testing.T, what string, limit time.Duration, fn func()) {
	t.Helper()
	done := make(chan struct{})
	start := time.Now()
	go func() { defer close(done); fn() }()
	select {
	case <-done:
		if d := time.Since(start); d > limit {
			t.Errorf("%s took %s, want under %s", what, d, limit)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("%s did not finish in 30s", what)
	}
}

// TestTxEarlyCloseIsBounded: closing a Tx result set early, or stopping at
// its Limit, reads at most a bounded number of rows and then stops the
// statement with KILL QUERY; the Tx stays usable.
func TestTxEarlyCloseIsBounded(t *testing.T) {
	c := openMy(t, false)
	for _, limit := range []int{0, 10} {
		tx := begin(t, c, driver.TxOptions{})
		rs, err := tx.Query(ctxT(t), driver.Query{Text: hugeQuery, Limit: limit})
		if err != nil {
			t.Fatal(err)
		}
		within(t, fmt.Sprintf("early close (limit %d)", limit), 2*time.Second, func() {
			for range 10 {
				if _, ok, err := rs.Next(ctxT(t)); !ok || err != nil {
					t.Errorf("Next: %v %v", ok, err)
					return
				}
			}
			if limit > 0 {
				if _, ok, err := rs.Next(ctxT(t)); ok || err != nil {
					t.Errorf("Next past the limit: %v %v", ok, err)
				}
			}
			if err := rs.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		})
		if limit > 0 && !rs.Stats().Truncated {
			t.Error("Truncated not set")
		}
		rows, _, _ := collect(t, tx, driver.Query{Text: "SELECT 1"})
		if len(rows) != 1 {
			t.Errorf("Tx unusable after an early close: %v", rows)
		}
		if err := tx.Commit(ctxT(t)); err != nil {
			t.Errorf("Commit after an early close: %v", err)
		}
	}
}

// TestTxNextCancelled: a cancelled Next ctx stops the statement at once.
func TestTxNextCancelled(t *testing.T) {
	c := openMy(t, false)
	tx := begin(t, c, driver.TxOptions{})
	rs, err := tx.Query(ctxT(t), driver.Query{Text: hugeQuery})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := rs.Next(ctxT(t)); !ok || err != nil {
		t.Fatalf("Next: %v %v", ok, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	within(t, "Next with a cancelled ctx", 2*time.Second, func() {
		if _, _, err := rs.Next(ctx); !errors.Is(err, context.Canceled) {
			t.Errorf("Next(cancelled): want context.Canceled, got %v", err)
		}
		_ = rs.Close()
	})
	rows, _, _ := collect(t, tx, driver.Query{Text: "SELECT 1"})
	if len(rows) != 1 {
		t.Errorf("Tx unusable after a cancelled Next: %v", rows)
	}
}

// TestTxEndWithReaderMidIteration: Rollback and the Conn's Close, while
// another goroutine is reading a Tx result set, never drain it; Commit closes
// it through the bounded early close and commits. None of them hangs, races
// or panics (run with -race).
func TestTxEndWithReaderMidIteration(t *testing.T) {
	db := admin(t)
	name := scratch(t, db)
	before := tableState(t, db, name)
	for _, end := range []string{"rollback", "close", "commit"} { // commit last: it changes the table
		t.Run(end, func(t *testing.T) {
			c, err := New().Open(context.Background(), driver.ConnConfig{DSN: testDSN})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.Close() }()
			txi, err := c.(driver.Transactor).Begin(ctxT(t), driver.TxOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if err := queryErr(t, txi, driver.Query{Text: "DELETE FROM " + name}); err != nil {
				t.Fatal(err)
			}
			rs, err := txi.Query(ctxT(t), driver.Query{Text: hugeQuery})
			if err != nil {
				t.Fatal(err)
			}
			started := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; ; i++ {
					if i == 5 {
						close(started)
					}
					if _, ok, err := rs.Next(context.Background()); !ok || err != nil {
						if i < 5 {
							close(started)
						}
						return
					}
				}
			}()
			<-started
			within(t, end+" with a reader mid-iteration", 3*time.Second, func() {
				var err error
				switch end {
				case "rollback":
					err = txi.Rollback(context.Background())
				case "commit":
					err = txi.Commit(context.Background())
				case "close":
					err = c.Close()
				}
				if err != nil {
					t.Errorf("%s: %v", end, err)
				}
			})
			within(t, "the reader to stop", 3*time.Second, wg.Wait)
			_ = rs.Close()
			got := tableState(t, db, name)
			if end == "commit" && got != "0 rows, checksum 0" {
				t.Errorf("Commit lost the DELETE: %s", got)
			}
			if end != "commit" && got != before {
				t.Errorf("%s committed the Tx: %s, was %s", end, got, before)
			}
		})
	}
}

// TestTxCommitWithOpenResult: Commit closes an open result set over a huge
// result through the bounded early close (no full drain) and still commits
// the Tx's earlier writes.
func TestTxCommitWithOpenResult(t *testing.T) {
	db := admin(t)
	name := scratch(t, db)
	c := openMy(t, false)
	tx := begin(t, c, driver.TxOptions{})
	if err := queryErr(t, tx, driver.Query{Text: "UPDATE " + name + " SET age = 4242 WHERE id = 1"}); err != nil {
		t.Fatal(err)
	}
	rs, err := tx.Query(ctxT(t), driver.Query{Text: hugeQuery})
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, ok, err := rs.Next(ctxT(t)); !ok || err != nil {
			t.Fatalf("Next: %v %v", ok, err)
		}
	}
	within(t, "Commit with an open result set", 2*time.Second, func() {
		if err := tx.Commit(ctxT(t)); err != nil {
			t.Errorf("Commit: %v", err)
		}
	})
	if _, ok, err := rs.Next(ctxT(t)); ok || err != nil {
		t.Errorf("Next after Commit: %v %v", ok, err)
	}
	_ = rs.Close()
	var age int
	if err := db.QueryRow("SELECT age FROM " + name + " WHERE id = 1").Scan(&age); err != nil || age != 4242 {
		t.Errorf("committed age seen from another connection = %d, %v; want 4242", age, err)
	}
}

// TestTxDeadlockIsConflict: the deadlock victim gets ErrConflict, and its Tx
// (rolled back by the server) is broken.
func TestTxDeadlockIsConflict(t *testing.T) {
	db := admin(t)
	name := scratch(t, db)
	c := openMy(t, false)
	a := begin(t, c, driver.TxOptions{})
	b := begin(t, c, driver.TxOptions{})
	upd := func(tx *myTx, id int) error {
		return queryErr(t, tx, driver.Query{Text: fmt.Sprintf("UPDATE %s SET age = age + 1 WHERE id = %d", name, id)})
	}
	if err := upd(a, 1); err != nil {
		t.Fatal(err)
	}
	if err := upd(b, 2); err != nil {
		t.Fatal(err)
	}
	errA := make(chan error, 1)
	go func() { errA <- upd(a, 2) }() // waits for b's lock
	time.Sleep(300 * time.Millisecond)
	errB := upd(b, 1) // deadlock: one of the two is the victim
	ea := <-errA
	victim, verr := a, ea
	if errB != nil {
		victim, verr = b, errB
		if ea != nil {
			t.Fatalf("both failed: %v / %v", ea, errB)
		}
	} else if ea == nil {
		t.Fatal("no deadlock")
	}
	var de *driver.Error
	if !errors.Is(verr, driver.ErrConflict) || !errors.As(verr, &de) || de.Code != "1213" {
		t.Errorf("victim: want ErrConflict (1213), got %v", verr)
	}
	if err := queryErr(t, victim, driver.Query{Text: "SELECT 1"}); !errors.Is(err, driver.ErrConnection) {
		t.Errorf("victim Tx must be broken, got %v", err)
	}
	if err := victim.Commit(ctxT(t)); err == nil {
		t.Error("Commit of the victim Tx succeeded")
	}
}

// TestTxConcurrentCalls: while one call runs on the Tx connection, a second
// one is refused instead of sharing the connection.
func TestTxConcurrentCalls(t *testing.T) {
	db := admin(t)
	name := scratch(t, db)
	lock, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lock.Exec("SELECT * FROM " + name + " WHERE id = 1 FOR UPDATE"); err != nil {
		t.Fatal(err)
	}
	c := openMy(t, false)
	tx := begin(t, c, driver.TxOptions{})
	done := make(chan error, 1)
	go func() { // blocks on the row lock
		done <- tx.Update(context.Background(), driver.CollectionRef{Name: name}, driver.Object{{Key: "id", Value: driver.Int(1)}}, driver.Object{{Key: "age", Value: driver.Int(5)}})
	}()
	time.Sleep(300 * time.Millisecond)
	if _, err := tx.Query(ctxT(t), driver.Query{Text: "SELECT 1"}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("Query during a running mutation: want ErrUnsupported, got %v", err)
	}
	if err := tx.Delete(ctxT(t), driver.CollectionRef{Name: name}, driver.Object{{Key: "id", Value: driver.Int(2)}}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("Delete during a running mutation: want ErrUnsupported, got %v", err)
	}
	_ = lock.Rollback()
	if err := <-done; err != nil {
		t.Errorf("Update: %v", err)
	}
}

// TestTxRefusesQuotedAutocommit: every spelling of autocommit is refused, and
// the Tx stays open.
func TestTxRefusesQuotedAutocommit(t *testing.T) {
	c := openMy(t, false)
	tx := begin(t, c, driver.TxOptions{})
	for _, text := range []string{
		"SET `autocommit` = 1", "SET @@`autocommit` = 1", "SET @@session.autocommit = 1", "SET @@session.`autocommit` = 1",
		"SET SESSION autocommit = 1", "SET SESSION `autocommit` = 1", "SET @x = 1, `AutoCommit` = 1",
		"COMMIT AND CHAIN", "COMMIT WORK AND NO CHAIN", "ROLLBACK AND CHAIN",
	} {
		if err := queryErr(t, tx, driver.Query{Text: text}); !errors.Is(err, driver.ErrUnsupported) {
			t.Errorf("Tx.Query(%q): want ErrUnsupported, got %v", text, err)
		}
	}
	rows, _, _ := collect(t, tx, driver.Query{Text: "SELECT @@autocommit"})
	if rows[0][0].String() != "1" {
		t.Errorf("autocommit changed: %v", rows[0])
	}
	if err := tx.Commit(ctxT(t)); err != nil {
		t.Errorf("Commit: %v", err)
	}
}

// TestRollbackReleasesLocksAtOnce: once Rollback returns, the Tx's row
// locks are gone, so another connection can lock the row without waiting.
func TestRollbackReleasesLocksAtOnce(t *testing.T) {
	db := admin(t)
	name := "tobira_rollback_big" // big enough that undoing the update takes a while
	mustExec(t, db, "CREATE TABLE "+name+" (id INT PRIMARY KEY, age INT)")
	t.Cleanup(func() { mustExec(t, db, "DROP TABLE "+name) })
	mustExec(t, db, "INSERT INTO "+name+" SELECT a.n * 1000 + b.n, 0 FROM tobira_big a JOIN tobira_big b ON b.n <= 200")
	c := openMy(t, false)
	for i := range 2 {
		tx := begin(t, c, driver.TxOptions{})
		if err := queryErr(t, tx, driver.Query{Text: "UPDATE " + name + " SET age = age + 1"}); err != nil {
			t.Fatal(err)
		}
		if err := tx.Rollback(ctxT(t)); err != nil {
			t.Fatal(err)
		}
		other, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		_, err = other.Exec("SELECT * FROM " + name + " WHERE id = 1001 FOR UPDATE NOWAIT")
		_ = other.Rollback()
		if err != nil {
			t.Fatalf("round %d: rows still locked after Rollback returned: %v", i, err)
		}
	}
}

// TestEarlyCloseWithoutAPoolConnection: when KILL QUERY cannot get a pooled
// connection quickly, the early close does not fall back to reading the rest:
// it closes the Tx connection's socket and the Tx is dead.
func TestEarlyCloseWithoutAPoolConnection(t *testing.T) {
	c := openMy(t, false)
	tx := begin(t, c, driver.TxOptions{})
	c.db.SetMaxOpenConns(1) // the Tx holds the only connection
	t.Cleanup(func() { c.db.SetMaxOpenConns(0) })
	rs, err := tx.Query(ctxT(t), driver.Query{Text: hugeQuery})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := rs.Next(ctxT(t)); !ok || err != nil {
		t.Fatalf("Next: %v %v", ok, err)
	}
	within(t, "early close with an exhausted pool", 2*time.Second, func() { _ = rs.Close() })
	if err := queryErr(t, tx, driver.Query{Text: "SELECT 1"}); !errors.Is(err, driver.ErrConnection) {
		t.Errorf("Tx after a failed kill: want ErrConnection (dead), got %v", err)
	}
}

// TestCommitWhileMutationRuns: Commit refuses while a mutation is running;
// Rollback stops it and rolls back.
func TestCommitWhileMutationRuns(t *testing.T) {
	db := admin(t)
	name := scratch(t, db)
	before := tableState(t, db, name)
	lock, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Rollback() }()
	if _, err := lock.Exec("SELECT * FROM " + name + " WHERE id = 1 FOR UPDATE"); err != nil {
		t.Fatal(err)
	}
	c := openMy(t, false)
	tx := begin(t, c, driver.TxOptions{})
	if err := queryErr(t, tx, driver.Query{Text: "DELETE FROM " + name + " WHERE id = 2"}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { // blocks on the row lock
		done <- tx.Update(context.Background(), driver.CollectionRef{Name: name}, driver.Object{{Key: "id", Value: driver.Int(1)}}, driver.Object{{Key: "age", Value: driver.Int(5)}})
	}()
	time.Sleep(300 * time.Millisecond)
	if err := tx.Commit(ctxT(t)); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("Commit during a running mutation: want ErrUnsupported, got %v", err)
	}
	within(t, "Rollback during a running mutation", 3*time.Second, func() {
		if err := tx.Rollback(ctxT(t)); err != nil {
			t.Errorf("Rollback: %v", err)
		}
	})
	select {
	case err := <-done:
		if err == nil {
			t.Error("the stopped call reported success")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the mutation did not stop after Rollback")
	}
	_ = lock.Rollback()
	if got := tableState(t, db, name); got != before {
		t.Errorf("the Tx was committed: %s, was %s", got, before)
	}
}

// TestCommitWhileQueryOpens: Commit refuses while a statement is still
// executing (no rows yet); Rollback stops it.
func TestCommitWhileQueryOpens(t *testing.T) {
	c := openMy(t, false)
	tx := begin(t, c, driver.TxOptions{})
	done := make(chan error, 1)
	// No t helpers off the test goroutine: ctx is made here.
	ctx := ctxT(t)
	go func() {
		rs, err := tx.Query(ctx, driver.Query{Text: "SELECT SLEEP(5)"})
		if err == nil {
			_, _, err = driver.Collect(ctx, rs, 0)
		}
		done <- err
	}()
	// Wait until the server is executing the statement on the Tx connection.
	db := admin(t)
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.PROCESSLIST WHERE ID = ? AND INFO LIKE 'SELECT SLEEP%'", tx.connID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the statement never started on the server")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := tx.Commit(ctxT(t)); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("Commit during a running statement: want ErrUnsupported, got %v", err)
	}
	within(t, "Rollback during a running statement", 3*time.Second, func() { _ = tx.Rollback(ctxT(t)) })
	select {
	case err := <-done:
		if err == nil {
			t.Error("the stopped call reported success")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the statement did not stop after Rollback")
	}
}
