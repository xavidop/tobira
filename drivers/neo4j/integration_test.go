//go:build integration

package neo4j

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
	tcneo4j "github.com/testcontainers/testcontainers-go/modules/neo4j"
	"go.uber.org/goleak"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

// image is the Neo4j image the integration suite runs against: the 5.x
// LTS line, or TOBIRA_TEST_NEO4J_IMAGE.
func image() string {
	if img := os.Getenv("TOBIRA_TEST_NEO4J_IMAGE"); img != "" {
		return img
	}
	return "neo4j:5.26"
}

const (
	testUser     = "neo4j"
	testPassword = "tobira-password"
	testDB       = "neo4j" // Community Edition has one user database
	people       = "tobira_people"
)

// testHostPort is the container's mapped Bolt address; admin is a Go
// driver on it for seeding and checks, outside the tobira driver.
var (
	testHostPort string
	admin        neo4j.DriverWithContext
	leakBaseline goleak.Option
)

func TestMain(m *testing.M) { os.Exit(runMain(m)) }

func runMain(m *testing.M) int {
	ctx := context.Background()
	ctr, err := tcneo4j.Run(ctx, image(), tcneo4j.WithAdminPassword(testPassword))
	if err != nil {
		fmt.Fprintln(os.Stderr, "start neo4j container:", err)
		return 1
	}
	defer func() { _ = ctr.Terminate(ctx) }()
	bolt, err := ctr.BoltUrl(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bolt url:", err)
		return 1
	}
	testHostPort = strings.TrimPrefix(bolt, "bolt://")
	testHostPort = strings.TrimPrefix(testHostPort, "neo4j://")
	testHostPort = strings.Replace(testHostPort, "localhost:", "127.0.0.1:", 1)
	admin, err = neo4j.NewDriverWithContext("bolt://"+testHostPort, neo4j.BasicAuth(testUser, testPassword, ""))
	if err != nil {
		fmt.Fprintln(os.Stderr, "admin driver:", err)
		return 1
	}
	if err := seed(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "seed:", err)
		_ = admin.Close(ctx)
		return 1
	}
	leakBaseline = goleak.IgnoreCurrent()
	code := m.Run()
	_ = admin.Close(ctx)
	if code == 0 {
		if err := goleak.Find(leakBaseline); err != nil {
			fmt.Fprintln(os.Stderr, "goroutine leak:", err)
			code = 1
		}
	}
	return code
}

// exec runs cypher through the admin driver.
func exec(ctx context.Context, cypher string, ps map[string]any) error {
	_, err := neo4j.ExecuteQuery(ctx, admin, cypher, ps, neo4j.EagerResultTransformer, neo4j.ExecuteQueryWithDatabase(testDB))
	if err != nil {
		return fmt.Errorf("%.60s: %w", cypher, err)
	}
	return nil
}

// seed loads People as nodes with the label tobira_people, each property
// as the Go driver stores it: score as a float (Neo4j has no decimal),
// tags as a list of strings (no JSON), and a KNOWS relationship from each
// person to the next. The leak probe sleeps through a function of its
// own, so a slow query exists without a plugin.
func seed(ctx context.Context) error {
	if err := exec(ctx, "MATCH (n) DETACH DELETE n", nil); err != nil {
		return err
	}
	cols := drivertest.People.Columns
	for _, row := range drivertest.People.Rows {
		props := map[string]any{}
		for i, col := range cols {
			if row[i].IsNull() {
				continue
			}
			v, err := encode(row[i])
			if err != nil {
				return err
			}
			props[col.Name] = v
		}
		if err := exec(ctx, "CREATE (p:"+quote(people)+") SET p = $props", map[string]any{"props": props}); err != nil {
			return err
		}
	}
	if err := exec(ctx, "MATCH (a:"+quote(people)+"), (b:"+quote(people)+") WHERE b.id = a.id + 1 CREATE (a)-[:KNOWS {since: a.id}]->(b)", nil); err != nil {
		return err
	}
	return exec(ctx, "CREATE (:tobira_team {name: 'core'})-[:HAS_MEMBER]->(:tobira_people_shadow {id: 1})", nil)
}

// dsnFor returns a DSN for the admin user with the given password.
func dsnFor(password, params string) string {
	dsn := "bolt://" + testUser
	if password != "" {
		dsn += ":" + password
	}
	dsn += "@" + testHostPort + "/" + testDB
	if params != "" {
		dsn += "?" + params
	}
	return dsn
}

// openTest opens the driver and closes it with the test.
func openTest(t testing.TB, readOnly bool) driver.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := New().Open(ctx, driver.ConnConfig{DSN: dsnFor(testPassword, ""), ReadOnly: readOnly})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}
