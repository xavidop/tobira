//go:build integration

package dynamodb

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.uber.org/goleak"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

// image is the pinned DynamoDB Local image.
const image = "amazon/dynamodb-local:3.3.2"

const (
	testKeyID  = "tobiratest"
	testSecret = "tobira-secret-access-key-0123456789"
	testRegion = "us-east-1"
)

// endpoint is the DynamoDB Local container shared by every test in the
// package (run with -sharedDb, so every access key sees the same tables);
// admin is a plain SDK client on it for seeding and checks, on its own
// transport so its keep-alive connections can be closed before the leak check.
var (
	endpoint       string
	admin          *dynamodb.Client
	adminTransport = http.DefaultTransport.(*http.Transport).Clone()
	leakBaseline   goleak.Option
)

func TestMain(m *testing.M) { os.Exit(runMain(m)) }

func runMain(m *testing.M) int {
	ctx := context.Background()
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        image,
			Cmd:          []string{"-jar", "DynamoDBLocal.jar", "-inMemory", "-sharedDb"},
			ExposedPorts: []string{"8000/tcp"},
			WaitingFor:   wait.ForListeningPort("8000/tcp").WithStartupTimeout(2 * time.Minute),
		},
		Started: true,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "start dynamodb-local:", err)
		return 1
	}
	defer func() { _ = testcontainers.TerminateContainer(ctr) }()
	hp, err := ctr.PortEndpoint(ctx, "8000/tcp", "http")
	if err != nil {
		fmt.Fprintln(os.Stderr, "endpoint:", err)
		return 1
	}
	endpoint = hp
	admin = dynamodb.New(dynamodb.Options{
		Region:       testRegion,
		BaseEndpoint: aws.String(endpoint),
		Credentials:  credentials.NewStaticCredentialsProvider("admin", "admin-secret", ""),
		HTTPClient:   &http.Client{Transport: adminTransport},
	})
	if err := waitReady(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "dynamodb-local not ready:", err)
		return 1
	}
	if err := seedPeople(ctx, drivertest.PeopleCollection()); err != nil {
		fmt.Fprintln(os.Stderr, "seed:", err)
		return 1
	}
	leakBaseline = goleak.IgnoreCurrent()
	code := m.Run()
	adminTransport.CloseIdleConnections()
	if code == 0 {
		if err := goleak.Find(leakBaseline); err != nil {
			fmt.Fprintln(os.Stderr, "goroutine leak:", err)
			code = 1
		}
	}
	return code
}

func waitReady(ctx context.Context) error {
	var err error
	for range 60 {
		if _, err = admin.ListTables(ctx, &dynamodb.ListTablesInput{}); err == nil {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return err
}

func dsn() string {
	return "dynamodb://" + testKeyID + "@" + testRegion + "?endpoint=" + endpoint
}

func cfg(readOnly bool) driver.ConnConfig {
	return driver.ConnConfig{DSN: dsn(), Password: driver.NewSecret(testSecret), ReadOnly: readOnly}
}

func ctx30(t testing.TB) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// openTest opens the driver and closes it with the test.
func openTest(t testing.TB, readOnly bool) *conn {
	t.Helper()
	c, err := New().Open(ctx30(t), cfg(readOnly))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c.(*conn)
}

// createTable creates a pay-per-request table keyed by pk (and sk when
// skType is not ""), optionally with a stream and GSIs, dropping any
// previous one.
func createTable(ctx context.Context, name, pk, pkType, sk, skType string, stream bool, gsis ...types.GlobalSecondaryIndex) error {
	_, _ = admin.DeleteTable(ctx, &dynamodb.DeleteTableInput{TableName: aws.String(name)})
	attrs := []types.AttributeDefinition{{AttributeName: aws.String(pk), AttributeType: types.ScalarAttributeType(pkType)}}
	keys := []types.KeySchemaElement{{AttributeName: aws.String(pk), KeyType: types.KeyTypeHash}}
	if skType != "" {
		attrs = append(attrs, types.AttributeDefinition{AttributeName: aws.String(sk), AttributeType: types.ScalarAttributeType(skType)})
		keys = append(keys, types.KeySchemaElement{AttributeName: aws.String(sk), KeyType: types.KeyTypeRange})
	}
	in := &dynamodb.CreateTableInput{
		TableName: aws.String(name), AttributeDefinitions: attrs, KeySchema: keys,
		BillingMode: types.BillingModePayPerRequest,
	}
	for _, g := range gsis {
		for _, ks := range g.KeySchema {
			attrs = append(attrs, types.AttributeDefinition{AttributeName: ks.AttributeName, AttributeType: types.ScalarAttributeTypeS})
		}
		in.AttributeDefinitions = attrs
		in.GlobalSecondaryIndexes = append(in.GlobalSecondaryIndexes, g)
	}
	if stream {
		in.StreamSpecification = &types.StreamSpecification{StreamEnabled: aws.Bool(true), StreamViewType: types.StreamViewTypeNewAndOldImages}
	}
	_, err := admin.CreateTable(ctx, in)
	return err
}

// peopleItem is one People row as an item: id, age and score as N (a NULL
// age as NULL), created_at as an RFC 3339 S, tags as an L of S.
func peopleItem(r driver.Row) (item, error) {
	it := item{}
	for k, col := range drivertest.People.Columns {
		v := r[k]
		if col.Name == "created_at" {
			tm, _ := v.AsTime()
			it[col.Name] = &types.AttributeValueMemberS{Value: tm.UTC().Format(time.RFC3339)}
			continue
		}
		av, err := toAV(v)
		if err != nil {
			return nil, err
		}
		it[col.Name] = av
	}
	return it, nil
}

// seedPeople creates table name (key id, a stream, a GSI on email) and
// loads People.
func seedPeople(ctx context.Context, name string) error {
	gsi := types.GlobalSecondaryIndex{
		IndexName:  aws.String("by_email"),
		KeySchema:  []types.KeySchemaElement{{AttributeName: aws.String("email"), KeyType: types.KeyTypeHash}},
		Projection: &types.Projection{ProjectionType: types.ProjectionTypeAll},
	}
	if err := createTable(ctx, name, "id", "N", "", "", true, gsi); err != nil {
		return err
	}
	for _, r := range drivertest.People.Rows {
		it, err := peopleItem(r)
		if err != nil {
			return err
		}
		if _, err := admin.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String(name), Item: it}); err != nil {
			return err
		}
	}
	return nil
}

// put writes items into table with the admin client.
func put(t testing.TB, table string, items ...item) {
	t.Helper()
	for _, it := range items {
		if _, err := admin.PutItem(ctx30(t), &dynamodb.PutItemInput{TableName: aws.String(table), Item: it}); err != nil {
			t.Fatal(err)
		}
	}
}

func num(i int) types.AttributeValue    { return &types.AttributeValueMemberN{Value: strconv.Itoa(i)} }
func str(v string) types.AttributeValue { return &types.AttributeValueMemberS{Value: v} }

// runQuery runs a query and drains it.
func runQuery(t testing.TB, c driver.Querier, q driver.Query) ([]driver.Column, []driver.Row, driver.Stats) {
	t.Helper()
	rs, err := c.Query(ctx30(t), q)
	if err != nil {
		t.Fatalf("%s: %v", q.Text, err)
	}
	cols := rs.Columns()
	rows, st, err := driver.Collect(ctx30(t), rs, 0)
	if err != nil {
		t.Fatalf("%s: %v", q.Text, err)
	}
	return cols, rows, st
}

func browse(t testing.TB, b driver.Browser, table string, opts driver.BrowseOptions) ([]driver.Column, []driver.Row, driver.Stats, error) {
	t.Helper()
	rs, err := b.Browse(ctx30(t), driver.CollectionRef{Namespace: namespaceName, Name: table}, opts)
	if err != nil {
		return nil, nil, driver.Stats{}, err
	}
	cols := rs.Columns()
	rows, st, err := driver.Collect(ctx30(t), rs, 0)
	return cols, rows, st, err
}

func colIndex(t testing.TB, cols []driver.Column, name string) int {
	t.Helper()
	for i, c := range cols {
		if c.Name == name {
			return i
		}
	}
	t.Fatalf("no column %q in %v", name, cols)
	return -1
}
