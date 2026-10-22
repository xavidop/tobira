//go:build integration

package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

// The flavor server. SingleStore has no container here: its dev image is
// amd64-only and about 4 GB, too big for the test machines, so the
// singlestore flavor is tested forced onto mysql:8.4 and MariaDB
// (TestForcedFlavor*), plus unit tests of its dialect differences.
const mariaDBImage = "mariadb:11.8.9"

// mariaDSN and mariaHostPort reach the MariaDB container (root, database
// tobira).
var mariaDSN, mariaHostPort string

// flavorServers starts the flavor containers in the background.
type flavorServers struct {
	wg   sync.WaitGroup
	mu   sync.Mutex
	ctrs []testcontainers.Container
	errs []error
}

func startFlavorServers(ctx context.Context) *flavorServers {
	fs := &flavorServers{}
	fs.start(ctx, "mariadb", startMariaDB)
	return fs
}

func (fs *flavorServers) start(ctx context.Context, name string, run func(context.Context) (testcontainers.Container, error)) {
	fs.wg.Add(1)
	go func() {
		defer fs.wg.Done()
		ctr, err := run(ctx)
		fs.mu.Lock()
		defer fs.mu.Unlock()
		if ctr != nil {
			fs.ctrs = append(fs.ctrs, ctr)
		}
		if err != nil {
			fs.errs = append(fs.errs, fmt.Errorf("start %s container: %w", name, err))
		}
	}()
}

// wait blocks until every flavor server is up and seeded.
func (fs *flavorServers) wait() error {
	fs.wg.Wait()
	return errors.Join(fs.errs...)
}

func (fs *flavorServers) terminate() {
	fs.wg.Wait()
	for _, c := range fs.ctrs {
		_ = testcontainers.TerminateContainer(c)
	}
}

func startMariaDB(ctx context.Context) (testcontainers.Container, error) {
	ctr, err := testcontainers.Run(ctx, mariaDBImage,
		testcontainers.WithExposedPorts("3306/tcp"),
		testcontainers.WithEnv(map[string]string{"MARIADB_ROOT_PASSWORD": "tobira", "MARIADB_DATABASE": "tobira"}),
		testcontainers.WithWaitStrategy(
			wait.ForLog("ready for connections").WithOccurrence(2).WithStartupTimeout(2*time.Minute),
			wait.ForListeningPort("3306/tcp"),
		))
	if err != nil {
		return ctr, err
	}
	if mariaHostPort, err = ctr.PortEndpoint(ctx, "3306/tcp", ""); err != nil {
		return ctr, err
	}
	mariaDSN = "root:tobira@tcp(" + mariaHostPort + ")/tobira"
	return ctr, seedMariaDB(ctx)
}

func seedMariaDB(ctx context.Context) error {
	db, err := sql.Open("mysql", mariaDSN)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	if err := pingUntil(ctx, db, time.Minute); err != nil {
		return err
	}
	stmts := append(drivertest.SQLSeed("mysql"),
		"CREATE TABLE tobira_orders (id INT PRIMARY KEY AUTO_INCREMENT, person_id INT NOT NULL, "+
			"total DECIMAL(8,2) DEFAULT 0, doubled DECIMAL(9,2) GENERATED ALWAYS AS (total * 2) STORED, "+
			"note VARCHAR(50) DEFAULT 'n/a' COMMENT 'free text', created DATETIME DEFAULT CURRENT_TIMESTAMP, "+
			"ref UUID, ip INET6, "+
			"CONSTRAINT fk_orders_person FOREIGN KEY (person_id) REFERENCES tobira_people(id), "+
			"INDEX tobira_orders_person_total (person_id, total), UNIQUE INDEX tobira_orders_note (note)) COMMENT 'orders table'",
		"INSERT INTO tobira_orders (person_id, total, note, ref, ip) VALUES (1, 10.50, 'first', '123e4567-e89b-12d3-a456-426614174000', '::1')",
		"CREATE SEQUENCE tobira_seq",
		pairsTable,
		pairsRows,
	)
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("%.60s: %w", s, err)
		}
	}
	return nil
}

// pairsTable has a composite primary key, for keyset paging over two
// columns; pairsRows fills it with 25 rows.
const (
	pairsTable = "CREATE TABLE tobira_pairs (a INT NOT NULL, b VARCHAR(10) NOT NULL, v INT, PRIMARY KEY (a, b))"
	pairsRows  = "INSERT INTO tobira_pairs VALUES (1,'a',1),(1,'b',2),(1,'c',3),(2,'a',4),(2,'b',5),(3,'a',6),(3,'b',7),(3,'c',8),(3,'d',9)," +
		"(4,'a',10),(5,'a',11),(5,'b',12),(6,'a',13),(7,'a',14),(7,'b',15),(8,'a',16),(9,'a',17),(9,'b',18),(9,'c',19)," +
		"(10,'a',20),(10,'b',21),(11,'a',22),(12,'a',23),(12,'b',24),(13,'a',25)"
)

func pingUntil(ctx context.Context, db *sql.DB, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	for {
		err := db.PingContext(ctx)
		if err == nil || time.Now().After(deadline) {
			return err
		}
		time.Sleep(time.Second)
	}
}

// openMaria opens the driver on the MariaDB container.
func openMaria(t *testing.T, readOnly bool) *conn {
	t.Helper()
	return openDSN(t, mariaDSN, readOnly)
}

// mariaAdmin opens a plain database/sql handle on the MariaDB container.
func mariaAdmin(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("mysql", mariaDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// browsePairs pages through tobira_pairs three rows at a time and returns
// the "a/b" keys in order.
func browsePairs(t *testing.T, c *conn) []string {
	t.Helper()
	_, rows, cursors := browseAll(t, c, driver.CollectionRef{Name: "tobira_pairs"}, driver.BrowseOptions{Limit: 3})
	if len(cursors) == 0 || !strings.HasPrefix(cursors[0], "k:") {
		t.Fatalf("composite key not keyset-paged: cursors %v", cursors)
	}
	keys := make([]string, len(rows))
	for i, r := range rows {
		keys[i] = r[0].String() + "/" + r[1].String()
	}
	return keys
}

// pairsKeys are tobira_pairs' keys in primary key order.
var pairsKeys = []string{"1/a", "1/b", "1/c", "2/a", "2/b", "3/a", "3/b", "3/c", "3/d", "4/a", "5/a", "5/b", "6/a",
	"7/a", "7/b", "8/a", "9/a", "9/b", "9/c", "10/a", "10/b", "11/a", "12/a", "12/b", "13/a"}

// TestForcedFlavorOnMySQL forces each other flavor on mysql:8.4. The
// dialect paths run (a forced singlestore uses START TRANSACTION, expanded
// keyset comparisons and no Transactor), and read-only is always off,
// because the guard is only trusted on the family it was verified against.
func TestForcedFlavorOnMySQL(t *testing.T) {
	db := admin(t)
	mustExec(t, db, "DROP TABLE IF EXISTS tobira_pairs")
	mustExec(t, db, pairsTable)
	mustExec(t, db, pairsRows)
	t.Cleanup(func() { mustExec(t, db, "DROP TABLE IF EXISTS tobira_pairs") })

	for _, f := range []string{"mariadb", "singlestore"} {
		t.Run(f, func(t *testing.T) {
			dsn := testDSN + "?flavor=" + f
			_, err := New().Open(ctxT(t), driver.ConnConfig{DSN: dsn, ReadOnly: true})
			if !errors.Is(err, driver.ErrUnsupported) {
				t.Fatalf("read-only open with flavor=%s on MySQL: want ErrUnsupported, got %v", f, err)
			}
			c := openDSN(t, dsn, false)
			if c.flavor.String() != f || c.detected != flavorMySQL {
				t.Fatalf("flavor %v, detected %v", c.flavor, c.detected)
			}
			if err := queryErr(t, c, driver.Query{Text: "SELECT 1", ReadOnly: true}); !errors.Is(err, driver.ErrUnsupported) {
				t.Errorf("per-call ReadOnly: want ErrUnsupported, got %v", err)
			}
			if got := browsePairs(t, c); fmt.Sprint(got) != fmt.Sprint(pairsKeys) {
				t.Errorf("browse = %v, want %v", got, pairsKeys)
			}
			s, err := c.Describe(ctxT(t), driver.CollectionRef{Name: "tobira_orders"})
			if err != nil || len(s.PrimaryKey) != 1 || len(s.ForeignKeys) != 1 {
				t.Errorf("Describe = %+v, %v", s, err)
			}
			name := scratch(t, db)
			ref := driver.CollectionRef{Name: name}
			if _, err := c.Insert(ctxT(t), ref, personRow(500, "forced@x")); err != nil {
				t.Fatalf("Insert: %v", err)
			}
			if err := c.Update(ctxT(t), ref, driver.Object{{Key: "id", Value: driver.Int(500)}}, driver.Object{{Key: "age", Value: driver.Int(9)}}); err != nil {
				t.Errorf("Update: %v", err)
			}
			if err := c.Delete(ctxT(t), ref, driver.Object{{Key: "id", Value: driver.Int(500)}}); err != nil {
				t.Errorf("Delete: %v", err)
			}
			tx, err := c.Begin(ctxT(t), driver.TxOptions{})
			switch {
			case f == "singlestore" && !errors.Is(err, driver.ErrUnsupported):
				t.Errorf("Begin with flavor=singlestore: want ErrUnsupported, got %v", err)
			case f == "mariadb" && err != nil:
				t.Errorf("Begin with flavor=mariadb: %v", err)
			case err == nil:
				_ = tx.Rollback(ctxT(t))
			}
		})
	}
}
