//go:build integration

package dynamodb

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
)

const (
	compTable  = "tobira_it_comp"
	typesTable = "tobira_it_types"
)

// seedComp creates compTable (pk S, sk N) with partitions a, b and c of
// 15 items each. name is set on odd sk and starts with "x" when sk < 5;
// tags is a string set holding "red" when sk is a multiple of 3.
func seedComp(t *testing.T) {
	t.Helper()
	if err := createTable(ctx30(t), compTable, "pk", "S", "sk", "N", false); err != nil {
		t.Fatal(err)
	}
	for _, pk := range []string{"a", "b", "c"} {
		for sk := 1; sk <= 15; sk++ {
			it := item{"pk": str(pk), "sk": num(sk)}
			if sk%2 == 1 {
				name := "y" + pk
				if sk < 5 {
					name = "x" + pk
				}
				it["name"] = str(name)
			}
			tags := []string{"blue"}
			if sk%3 == 0 {
				tags = append(tags, "red")
			}
			it["tags"] = &types.AttributeValueMemberSS{Value: tags}
			put(t, compTable, it)
		}
	}
}

func seedTypes(t *testing.T) {
	t.Helper()
	if err := createTable(ctx30(t), typesTable, "id", "S", "", "", false); err != nil {
		t.Fatal(err)
	}
	put(t, typesTable, item{
		"id":   str("all"),
		"b":    &types.AttributeValueMemberB{Value: []byte{0, 1, 255}},
		"big":  &types.AttributeValueMemberN{Value: "123456789012345678901234567890"},
		"dec":  &types.AttributeValueMemberN{Value: "0.125"},
		"t":    &types.AttributeValueMemberBOOL{Value: true},
		"z":    &types.AttributeValueMemberNULL{Value: true},
		"m":    &types.AttributeValueMemberM{Value: item{"k": str("v"), "n": &types.AttributeValueMemberM{Value: item{"deep": num(1)}}}},
		"l":    &types.AttributeValueMemberL{Value: []types.AttributeValue{str("a"), num(2)}},
		"ss":   &types.AttributeValueMemberSS{Value: []string{"b", "a"}},
		"ns":   &types.AttributeValueMemberNS{Value: []string{"10", "2"}},
		"bs":   &types.AttributeValueMemberBS{Value: [][]byte{{2}, {1}}},
		"text": str("héllo"),
	})
}

func TestIntegrationDescribe(t *testing.T) {
	seedComp(t)
	seedTypes(t)
	c := openTest(t, false)
	sch, err := c.Describe(ctx30(t), driver.CollectionRef{Namespace: namespaceName, Name: drivertest.PeopleCollection()})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sch.PrimaryKey, []string{"id"}) || !sch.Schemaless || len(sch.Sample) == 0 {
		t.Errorf("people: key %v schemaless %v sample %d", sch.PrimaryKey, sch.Schemaless, len(sch.Sample))
	}
	if !slices.ContainsFunc(sch.Indexes, func(i driver.Index) bool { return i.Name == "by_email" && i.Method == "gsi hash" }) ||
		!slices.ContainsFunc(sch.Indexes, func(i driver.Index) bool { return i.Primary && slices.Equal(i.Fields, []string{"id"}) }) {
		t.Errorf("people indexes: %+v", sch.Indexes)
	}
	if !strings.Contains(sch.Comment, "stream NEW_AND_OLD_IMAGES") {
		t.Errorf("people comment: %q", sch.Comment)
	}
	sch, err = c.Describe(ctx30(t), driver.CollectionRef{Name: compTable})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sch.PrimaryKey, []string{"pk", "sk"}) || sch.Fields[0].Name != "pk" || sch.Fields[1].Name != "sk" {
		t.Errorf("comp: key %v fields %+v", sch.PrimaryKey, sch.Fields[:2])
	}
	sch, err = c.Describe(ctx30(t), driver.CollectionRef{Name: typesTable})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		typ    driver.Type
		native string
	}{
		"id": {driver.TypeString, "S"}, "b": {driver.TypeBytes, "B"}, "big": {driver.TypeDecimal, "N"},
		"dec": {driver.TypeDecimal, "N"}, "t": {driver.TypeBool, "BOOL"}, "z": {driver.TypeNull, "NULL"},
		"m": {driver.TypeObject, "M"}, "l": {driver.TypeList, "L"}, "ss": {driver.TypeList, "SS"},
		"ns": {driver.TypeList, "NS"}, "bs": {driver.TypeList, "BS"}, "text": {driver.TypeString, "S"},
	}
	for _, f := range sch.Fields {
		if w := want[f.Name]; f.Type != w.typ || f.NativeType != w.native {
			t.Errorf("types.%s: %s %q, want %s %q", f.Name, f.Type, f.NativeType, w.typ, w.native)
		}
		if f.Name == "m" && (len(f.Fields) != 2 || f.Fields[1].Name != "n" || len(f.Fields[1].Fields) != 1) {
			t.Errorf("nested fields of m: %+v", f.Fields)
		}
	}
	if _, err := c.Describe(ctx30(t), driver.CollectionRef{Namespace: "other", Name: compTable}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("another namespace: want ErrNotFound, got %v", err)
	}
	cols, err := c.Collections(ctx30(t), driver.NamespaceRef{Name: namespaceName})
	if err != nil || !slices.ContainsFunc(cols, func(x driver.Collection) bool { return x.Ref.Name == compTable }) {
		t.Errorf("collections: %v %v", cols, err)
	}
}

func TestIntegrationQuery(t *testing.T) {
	seedComp(t)
	seedTypes(t)
	c := openTest(t, false)
	t.Run("projection order, key flags and params", func(t *testing.T) {
		cols, rows, _ := runQuery(t, c, driver.Query{Text: `SELECT sk, pk, nope FROM "tobira_it_comp" WHERE pk = ? AND sk = ?`,
			Params: []driver.Value{driver.String("b"), driver.Int(3)}})
		if len(cols) != 3 || cols[0].Name != "sk" || cols[1].Name != "pk" || cols[2].Name != "nope" {
			t.Fatalf("columns %+v", cols)
		}
		if !cols[0].Key || !cols[1].Key || cols[2].Key || cols[0].Source == nil || cols[0].Source.Name != compTable {
			t.Errorf("key flags / source: %+v", cols)
		}
		if len(rows) != 1 || !drivertest.EqualValue(driver.Int(3), rows[0][0]) || !rows[0][2].IsNull() {
			t.Errorf("rows %v", rows)
		}
	})
	t.Run("types", func(t *testing.T) {
		cols, rows, _ := runQuery(t, c, driver.Query{Text: `SELECT * FROM "tobira_it_types"`})
		if len(rows) != 1 {
			t.Fatalf("%d rows", len(rows))
		}
		got := map[string]driver.Value{}
		for i, col := range cols {
			got[col.Name] = rows[0][i]
		}
		want := map[string]driver.Value{
			"b": driver.Bytes([]byte{0, 1, 255}), "big": driver.Decimal("123456789012345678901234567890"),
			"dec": driver.Decimal("0.125"), "t": driver.Bool(true), "z": driver.Null(),
			"m": driver.ObjectValue(driver.Member{Key: "k", Value: driver.String("v")},
				driver.Member{Key: "n", Value: driver.ObjectValue(driver.Member{Key: "deep", Value: driver.Int(1)})}),
			"l":  driver.List(driver.String("a"), driver.Int(2)),
			"ss": driver.List(driver.String("a"), driver.String("b")),
			"ns": driver.List(driver.Int(2), driver.Int(10)),
			"bs": driver.List(driver.Bytes([]byte{1}), driver.Bytes([]byte{2})), "text": driver.String("héllo"),
		}
		for k, w := range want {
			if g := got[k]; g.Type() != w.Type() || !drivertest.EqualValue(w, g) {
				t.Errorf("%s: got %s (%s), want %s (%s)", k, g, g.Type(), w, w.Type())
			}
		}
	})
	t.Run("limit across pages", func(t *testing.T) {
		_, rows, st := runQuery(t, c, driver.Query{Text: `SELECT * FROM "tobira_it_comp"`, Limit: 10})
		if len(rows) != 10 || !st.Truncated {
			t.Errorf("%d rows, truncated %v", len(rows), st.Truncated)
		}
		_, rows, st = runQuery(t, c, driver.Query{Text: `SELECT * FROM "tobira_it_comp"`, Limit: 45})
		if len(rows) != 45 || st.Truncated {
			t.Errorf("limit 45: %d rows, truncated %v", len(rows), st.Truncated)
		}
	})
	t.Run("columns from the read-ahead, warning for the rest", func(t *testing.T) {
		put(t, compTable, item{"pk": str("w"), "sk": num(1)}, item{"pk": str("w"), "sk": num(2)},
			item{"pk": str("w"), "sk": num(3), "extra": str("late")})
		small := openTest(t, false)
		small.limits.prefetch = 2
		cols, rows, st := runQuery(t, small, driver.Query{Text: `SELECT * FROM "tobira_it_comp" WHERE pk = 'w' ORDER BY sk`})
		if len(rows) != 3 || len(cols) != 2 {
			t.Fatalf("%d rows, columns %+v: want 3 rows and the columns of the first two items", len(rows), cols)
		}
		if len(st.Warnings) != 1 || !strings.Contains(st.Warnings[0], "1 attributes of later items have no column") {
			t.Errorf("warnings %v", st.Warnings)
		}
		cols, _, st = runQuery(t, small, driver.Query{Text: `SELECT * FROM "tobira_it_comp" WHERE pk = 'w' ORDER BY sk DESC`})
		if len(cols) != 3 || len(st.Warnings) != 0 {
			t.Errorf("descending: columns %+v, warnings %v", cols, st.Warnings)
		}
	})
	t.Run("writes on a writable conn", func(t *testing.T) {
		_, rows, st := runQuery(t, c, driver.Query{Text: `INSERT INTO "tobira_it_comp" VALUE {'pk': 'q', 'sk': 1}`})
		if len(rows) != 0 || st.RowsAffected != -1 {
			t.Errorf("insert: %d rows, affected %d", len(rows), st.RowsAffected)
		}
		// The editor sends a row limit with every statement; a write must
		// still run.
		runQuery(t, c, driver.Query{Text: `UPDATE "tobira_it_comp" SET v = 2 WHERE pk = 'q' AND sk = 1`, Limit: 100})
		_, rows, _ = runQuery(t, c, driver.Query{Text: `SELECT * FROM "tobira_it_comp" WHERE pk = 'q'`, Limit: 100})
		if len(rows) != 1 {
			t.Errorf("inserted item: %d rows", len(rows))
		}
		_, err := c.Query(ctx30(t), driver.Query{Text: `INSERT INTO "tobira_it_comp" VALUE {'pk': 'q', 'sk': 1}`})
		if !errors.Is(err, driver.ErrConstraint) {
			t.Errorf("duplicate insert: want ErrConstraint, got %v", err)
		}
	})
	t.Run("errors", func(t *testing.T) {
		_, err := c.Query(ctx30(t), driver.Query{Text: `SELECT FROM WHERE`})
		if !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("bad statement: want ErrSyntax, got %v", err)
		}
		if err != nil && strings.Contains(err.Error(), testSecret) {
			t.Errorf("the secret is in the error: %v", err)
		}
		var de *driver.Error
		if !errors.As(err, &de) || de.Code != "ValidationException" || strings.Contains(err.Error(), "RequestID") {
			t.Errorf("want code ValidationException and no request id: %v", err)
		}
		if _, err := c.Query(ctx30(t), driver.Query{Text: `SELECT * FROM "x"`, Namespace: "other"}); !errors.Is(err, driver.ErrNotFound) {
			t.Errorf("another namespace: want ErrNotFound, got %v", err)
		}
	})
}

func TestIntegrationBrowse(t *testing.T) {
	seedComp(t)
	c := openTest(t, false)
	pageAll := func(t *testing.T, b *conn, opts driver.BrowseOptions) (sizes []int, keys []string) {
		t.Helper()
		for range 100 {
			cols, rows, st, err := browse(t, b, compTable, opts)
			if err != nil {
				t.Fatal(err)
			}
			pk, sk := colIndex(t, cols, "pk"), colIndex(t, cols, "sk")
			for _, r := range rows {
				keys = append(keys, r[pk].String()+"/"+r[sk].String())
			}
			sizes = append(sizes, len(rows))
			if st.NextCursor == "" {
				return sizes, keys
			}
			opts.Cursor = st.NextCursor
		}
		t.Fatal("still paging after 100 pages")
		return nil, nil
	}
	unique := func(t *testing.T, keys []string, n int) {
		t.Helper()
		s := slices.Clone(keys)
		slices.Sort(s)
		if len(slices.Compact(s)) != n || len(keys) != n {
			t.Errorf("got %d keys (%d distinct), want %d", len(keys), len(slices.Compact(s)), n)
		}
	}
	t.Run("keyset paging over a composite key", func(t *testing.T) {
		sizes, keys := pageAll(t, c, driver.BrowseOptions{Limit: 20})
		if !slices.Equal(sizes, []int{20, 20, 5}) {
			t.Errorf("sizes %v", sizes)
		}
		unique(t, keys, 45)
		cols, _, _, _ := browse(t, c, compTable, driver.BrowseOptions{Limit: 1})
		if cols[0].Name != "pk" || !cols[0].Key || !cols[1].Key || cols[0].Source == nil || cols[0].Source.Namespace != namespaceName {
			t.Errorf("columns %+v", cols)
		}
	})
	t.Run("filters", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			conds []driver.Condition
			want  int
		}{
			{"prefix", []driver.Condition{{Field: "name", Op: driver.OpPrefix, Value: driver.String("x")}}, 6},
			{"contains a set member", []driver.Condition{{Field: "tags", Op: driver.OpContains, Value: driver.String("red")}}, 15},
			{"in", []driver.Condition{{Field: "sk", Op: driver.OpIn, Value: driver.List(driver.Int(1), driver.Int(2))}}, 6},
			{"is_null", []driver.Condition{{Field: "name", Op: driver.OpIsNull}}, 21},
			{"not_null and gt", []driver.Condition{{Field: "name", Op: driver.OpNotNull}, {Field: "sk", Op: driver.OpGt, Value: driver.Int(10)}}, 9},
			{"eq on the partition", []driver.Condition{{Field: "pk", Op: driver.OpEq, Value: driver.String("b")}}, 15},
		} {
			sizes, keys := pageAll(t, c, driver.BrowseOptions{Limit: 4, Filter: driver.Filter{Conditions: tc.conds}})
			if len(keys) != tc.want {
				t.Errorf("%s: %d items (pages %v), want %d", tc.name, len(keys), sizes, tc.want)
			}
			unique(t, keys, tc.want)
		}
	})
	t.Run("sorted paging", func(t *testing.T) {
		var sks []int64
		opts := driver.BrowseOptions{Limit: 20, Sort: []driver.SortKey{{Field: "sk", Desc: true}}}
		for range 10 {
			cols, rows, st, err := browse(t, c, compTable, opts)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range rows {
				n, _ := r[colIndex(t, cols, "sk")].AsInt()
				sks = append(sks, n)
			}
			if st.NextCursor == "" {
				break
			}
			if !strings.HasPrefix(st.NextCursor, "o:") {
				t.Fatalf("cursor %q", st.NextCursor)
			}
			opts.Cursor = st.NextCursor
		}
		if len(sks) != 45 || !slices.IsSortedFunc(sks, func(a, b int64) int { return int(b - a) }) {
			t.Errorf("sorted sks %v", sks)
		}
	})
	t.Run("page byte budget", func(t *testing.T) {
		small := openTest(t, false)
		small.limits.resultBytes = 200
		_, rows, st, err := browse(t, small, compTable, driver.BrowseOptions{Limit: 20})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) >= 20 || st.NextCursor == "" || len(st.Warnings) != 1 {
			t.Errorf("%d rows, cursor %q, warnings %v", len(rows), st.NextCursor, st.Warnings)
		}
		_, keys := pageAll(t, small, driver.BrowseOptions{Limit: 20})
		unique(t, keys, 45)
	})
	t.Run("refusals", func(t *testing.T) {
		small := openTest(t, false)
		small.limits.sortItems = 10
		for name, tc := range map[string]struct {
			b    *conn
			opts driver.BrowseOptions
			kind error
		}{
			"raw filter":       {c, driver.BrowseOptions{Filter: driver.Filter{Raw: "pk = 'a'"}}, driver.ErrUnsupported},
			"offset unsorted":  {c, driver.BrowseOptions{Cursor: "o:20"}, driver.ErrSyntax},
			"garbage cursor":   {c, driver.BrowseOptions{Cursor: "k:zz"}, driver.ErrSyntax},
			"sort over cap":    {small, driver.BrowseOptions{Sort: []driver.SortKey{{Field: "sk"}}}, driver.ErrUnsupported},
			"contains on N sk": {c, driver.BrowseOptions{Filter: driver.Filter{Conditions: []driver.Condition{{Field: "sk", Op: driver.OpContains, Value: driver.String("1")}}}}, driver.ErrUnsupported},
		} {
			if _, _, _, err := browse(t, tc.b, compTable, tc.opts); !errors.Is(err, tc.kind) {
				t.Errorf("%s: want %v, got %v", name, tc.kind, err)
			}
		}
		_, _, st, _ := browse(t, c, compTable, driver.BrowseOptions{Limit: 5})
		if _, _, _, err := browse(t, c, compTable, driver.BrowseOptions{Cursor: st.NextCursor, Sort: []driver.SortKey{{Field: "sk"}}}); !errors.Is(err, driver.ErrSyntax) {
			t.Errorf("key cursor with a sort: want ErrSyntax, got %v", err)
		}
		if _, _, _, err := browse(t, c, "tobira_no_such_table", driver.BrowseOptions{}); !errors.Is(err, driver.ErrNotFound) {
			t.Errorf("missing table: want ErrNotFound, got %v", err)
		}
	})
}

func getItem(t *testing.T, table string, key item) item {
	t.Helper()
	out, err := admin.GetItem(ctx30(t), &dynamodb.GetItemInput{TableName: aws.String(table), Key: key, ConsistentRead: aws.Bool(true)})
	if err != nil {
		t.Fatal(err)
	}
	return out.Item
}

func TestIntegrationMutate(t *testing.T) {
	seedComp(t)
	c := openTest(t, false)
	ref := driver.CollectionRef{Namespace: namespaceName, Name: compTable}
	k := item{"pk": str("m"), "sk": num(1)}
	key := driver.Object{{Key: "pk", Value: driver.String("m")}, {Key: "sk", Value: driver.Int(1)}}
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	got, err := c.Insert(ctx30(t), ref, driver.Object{
		{Key: "sk", Value: driver.Int(1)}, {Key: "pk", Value: driver.String("m")}, {Key: "name", Value: driver.String("first")},
		{Key: "list", Value: driver.JSON([]byte(`["a",1]`))}, {Key: "at", Value: driver.Time(at)}, {Key: "nothing", Value: driver.Null()},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Key != "pk" || got[1].Key != "sk" {
		t.Errorf("insert key %v", got)
	}
	it := getItem(t, compTable, k)
	if nativeType(it["list"]) != "L" || it["at"].(*types.AttributeValueMemberS).Value != "2026-03-04T05:06:07Z" || nativeType(it["nothing"]) != "NULL" {
		t.Errorf("stored item %v", it)
	}
	if _, err := c.Insert(ctx30(t), ref, driver.Object{{Key: "pk", Value: driver.String("m")}, {Key: "sk", Value: driver.Int(1)}}); !errors.Is(err, driver.ErrConstraint) {
		t.Errorf("duplicate insert: want ErrConstraint, got %v", err)
	}
	if _, err := c.Insert(ctx30(t), ref, driver.Object{{Key: "pk", Value: driver.String("m")}}); !errors.Is(err, driver.ErrSyntax) {
		t.Errorf("insert without the sort key: want ErrSyntax, got %v", err)
	}
	if err := c.Update(ctx30(t), ref, key, driver.Object{{Key: "name", Value: driver.String("second")}, {Key: "sk", Value: driver.Int(1)}}); err != nil {
		t.Fatal(err)
	}
	if v := getItem(t, compTable, k)["name"].(*types.AttributeValueMemberS).Value; v != "second" {
		t.Errorf("after update: name %q", v)
	}
	for name, tc := range map[string]struct {
		key, changes driver.Object
		kind         error
	}{
		"key change":     {key, driver.Object{{Key: "sk", Value: driver.Int(2)}}, driver.ErrUnsupported},
		"partial key":    {driver.Object{{Key: "pk", Value: driver.String("m")}}, driver.Object{{Key: "name", Value: driver.String("x")}}, driver.ErrUnsupported},
		"stale field":    {append(slices.Clone(key), driver.Member{Key: "name", Value: driver.String("first")}), driver.Object{{Key: "name", Value: driver.String("x")}}, driver.ErrNotFound},
		"missing item":   {driver.Object{{Key: "pk", Value: driver.String("nope")}, {Key: "sk", Value: driver.Int(1)}}, driver.Object{{Key: "name", Value: driver.String("x")}}, driver.ErrNotFound},
		"no-op, missing": {driver.Object{{Key: "pk", Value: driver.String("nope")}, {Key: "sk", Value: driver.Int(1)}}, driver.Object{{Key: "sk", Value: driver.Int(1)}}, driver.ErrNotFound},
		"no changes":     {key, nil, driver.ErrSyntax},
	} {
		if err := c.Update(ctx30(t), ref, tc.key, tc.changes); !errors.Is(err, tc.kind) {
			t.Errorf("%s: want %v, got %v", name, tc.kind, err)
		}
	}
	if v := getItem(t, compTable, k)["name"].(*types.AttributeValueMemberS).Value; v != "second" {
		t.Errorf("a refused update changed name to %q", v)
	}
	if err := c.Update(ctx30(t), ref, append(slices.Clone(key), driver.Member{Key: "name", Value: driver.String("second")}),
		driver.Object{{Key: "name", Value: driver.Null()}}); err != nil {
		t.Errorf("update with a matching field: %v", err)
	}
	if nativeType(getItem(t, compTable, k)["name"]) != "NULL" {
		t.Error("a change to NULL must store NULL")
	}
	// A grid row shows the string set tags as a List; carried in the key,
	// it must still match the stored set.
	sk := item{"pk": str("a"), "sk": num(3)}
	setKey := driver.Object{{Key: "pk", Value: driver.String("a")}, {Key: "sk", Value: driver.Int(3)},
		{Key: "tags", Value: driver.List(driver.String("blue"), driver.String("red"))}}
	if err := c.Update(ctx30(t), ref, setKey, driver.Object{{Key: "name", Value: driver.String("set-matched")}}); err != nil {
		t.Errorf("update keyed by a set shown as a list: %v", err)
	}
	if v := getItem(t, compTable, sk)["name"].(*types.AttributeValueMemberS).Value; v != "set-matched" {
		t.Errorf("after the set-keyed update: name %q", v)
	}
	if err := c.Update(ctx30(t), ref, setKey, driver.Object{{Key: "sk", Value: driver.Int(3)}}); err != nil {
		t.Errorf("no-op update keyed by a set: %v", err)
	}
	staleSet := slices.Clone(setKey)
	staleSet[2].Value = driver.List(driver.String("blue"))
	if err := c.Update(ctx30(t), ref, staleSet, driver.Object{{Key: "name", Value: driver.String("x")}}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("a different set: want ErrNotFound, got %v", err)
	}
	if err := c.Delete(ctx30(t), ref, append(slices.Clone(key), driver.Member{Key: "name", Value: driver.String("x")})); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("delete with a stale field: want ErrNotFound, got %v", err)
	}
	if err := c.Delete(ctx30(t), ref, key); err != nil {
		t.Fatal(err)
	}
	if len(getItem(t, compTable, k)) != 0 {
		t.Error("item still there after Delete")
	}
	if err := c.Delete(ctx30(t), ref, key); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("second delete: want ErrNotFound, got %v", err)
	}
}

func TestIntegrationWatch(t *testing.T) {
	const table = "tobira_it_stream"
	seedComp(t)
	if err := createTable(ctx30(t), table, "pk", "S", "", "", true); err != nil {
		t.Fatal(err)
	}
	c := openTest(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := c.Watch(ctx, driver.WatchRequest{Channel: table})
	if err != nil {
		t.Fatal(err)
	}
	put(t, table, item{"pk": str("w"), "v": num(1)})
	put(t, table, item{"pk": str("w"), "v": num(2)})
	if _, err := admin.DeleteItem(ctx30(t), &dynamodb.DeleteItemInput{TableName: aws.String(table), Key: item{"pk": str("w")}}); err != nil {
		t.Fatal(err)
	}
	var got []string
	deadline := time.After(20 * time.Second)
	for len(got) < 3 {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatal("channel closed early")
			}
			if ev.Err != nil {
				t.Fatal(ev.Err)
			}
			pk, _ := ev.Key.Get("pk")
			v := "-"
			if obj, ok := ev.Payload.AsObject(); ok {
				x, _ := obj.Get("v")
				v = x.String()
			}
			got = append(got, fmt.Sprintf("%s %s %s %s", ev.Op, ev.Channel, pk, v))
		case <-deadline:
			t.Fatalf("got %v after 20 s", got)
		}
	}
	want := []string{"insert tobira_it_stream w 1", "update tobira_it_stream w 2", "delete tobira_it_stream w 2"}
	if !slices.Equal(got, want) {
		t.Errorf("events %q, want %q", got, want)
	}
	cancel()
	for range ch {
	}
	t.Run("ends when the connection closes", func(t *testing.T) {
		c2, err := New().Open(ctx30(t), cfg(false))
		if err != nil {
			t.Fatal(err)
		}
		ch, err := c2.(*conn).Watch(context.Background(), driver.WatchRequest{Channel: table})
		if err != nil {
			t.Fatal(err)
		}
		_ = c2.Close()
		deadline := time.After(3 * time.Second)
		for {
			select {
			case ev, ok := <-ch:
				if !ok {
					return
				}
				if ev.Err != nil {
					t.Errorf("closing the connection sent an error event: %v", ev.Err)
				}
			case <-deadline:
				t.Fatal("the watch channel is still open 3 s after Close")
			}
		}
	})
	if _, err := c.Watch(context.Background(), driver.WatchRequest{Collection: &driver.CollectionRef{Name: compTable}}); !errors.Is(err, driver.ErrUnsupported) {
		t.Errorf("table without a stream: want ErrUnsupported, got %v", err)
	}
	if _, err := c.Watch(context.Background(), driver.WatchRequest{Channel: "tobira_no_such_table"}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("missing table: want ErrNotFound, got %v", err)
	}
}
