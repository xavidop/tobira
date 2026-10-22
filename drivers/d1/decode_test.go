package d1

import (
	"bytes"
	"errors"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/xavidop/tobira/driver"
)

func rawBody(columns, rows string) []byte {
	return []byte(`{"result":[{"results":{"columns":` + columns + `,"rows":` + rows + `},"success":true,` +
		`"meta":{"changes":2,"last_row_id":9,"changed_db":true,"duration":0.1}}],"errors":[],"messages":[],"success":true}`)
}

func defaultBudget() *budget { return newBudget(resultBudgetBytes, resultBudgetCells) }

func TestDecodeRawResponse(t *testing.T) {
	data := rawBody(`["i","f","s","n","b","big","e","neg"]`,
		`[[1,1.5,"héllo",null,[0,255,7],9007199254740993,1e3,-4],[2,3,"",null,[],1,2.5e-3,0]]`)
	resp, err := decodeResponse(data, defaultBudget())
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Success || len(resp.Results) != 1 {
		t.Fatalf("response = %+v", resp)
	}
	r := resp.Results[0]
	if r.Meta.Changes != 2 || r.Meta.LastRowID != 9 {
		t.Errorf("meta = %+v", r.Meta)
	}
	if strings.Join(r.Columns, ",") != "i,f,s,n,b,big,e,neg" || len(r.Rows) != 2 {
		t.Fatalf("result = %+v", r)
	}
	row := r.Rows[0]
	want := []any{int64(1), 1.5, "héllo", nil, []byte{0, 255, 7}, int64(9007199254740993), 1000.0, int64(-4)}
	for i, w := range want {
		if b, ok := w.([]byte); ok {
			if got, _ := row[i].([]byte); !bytes.Equal(got, b) {
				t.Errorf("column %d = %#v, want %#v", i, row[i], w)
			}
			continue
		}
		if row[i] != w {
			t.Errorf("column %d = %#v (%T), want %#v (%T)", i, row[i], row[i], w, w)
		}
	}
	if got, ok := r.Rows[1][4].([]byte); !ok || len(got) != 0 {
		t.Errorf("empty blob = %#v", r.Rows[1][4])
	}
	if f, ok := r.Rows[1][1].(int64); !ok || f != 3 { // JS prints REAL 3.0 as 3
		t.Errorf("3 = %#v", r.Rows[1][1])
	}
}

func TestDecodeErrorEnvelope(t *testing.T) {
	long := strings.Repeat("é", 2000)
	data := []byte(`{"result":null,"success":false,"messages":[],"errors":[{"code":7500,"message":"no such table: nope: SQLITE_ERROR"},{"code":1,"message":"` + long + `"}]}`)
	resp, err := decodeResponse(data, defaultBudget())
	if err != nil {
		t.Fatal(err)
	}
	if resp.Success || len(resp.Errors) != 2 || resp.Errors[0].Code != 7500 || !strings.HasPrefix(resp.Errors[0].Message, "no such table") {
		t.Fatalf("response = %+v", resp)
	}
	if m := resp.Errors[1].Message; len(m) > maxErrorBytes+len("…") || !utf8.ValidString(m) {
		t.Errorf("long message not truncated on a rune boundary: %d bytes, valid %v", len(m), utf8.ValidString(m))
	}
	many := `{"success":false,"errors":[` + strings.Repeat(`{"code":1,"message":"x"},`, 100) + `{"code":2,"message":"y"}]}`
	resp, err = decodeResponse([]byte(many), defaultBudget())
	if err != nil || len(resp.Errors) > maxErrors {
		t.Errorf("errors not capped: %d, %v", len(resp.Errors), err)
	}
}

// TestDecodeRefusesHostileShapes: anything outside D1's value shapes, and any
// structural surprise, is an error, never a panic.
func TestDecodeRefusesHostileShapes(t *testing.T) {
	bad := map[string]string{
		"object value":         string(rawBody(`["a"]`, `[[{"x":1}]]`)),
		"bool value":           string(rawBody(`["a"]`, `[[true]]`)),
		"nested array in blob": string(rawBody(`["a"]`, `[[[[1]]]]`)),
		"blob byte 256":        string(rawBody(`["a"]`, `[[[256]]]`)),
		"blob byte -1":         string(rawBody(`["a"]`, `[[[-1]]]`)),
		"blob byte 1.5":        string(rawBody(`["a"]`, `[[[1.5]]]`)),
		"blob string":          string(rawBody(`["a"]`, `[[["x"]]]`)),
		"short row":            string(rawBody(`["a","b"]`, `[[1]]`)),
		"long row":             string(rawBody(`["a"]`, `[[1,2]]`)),
		"row not array":        string(rawBody(`["a"]`, `[1]`)),
		"rows not array":       string(rawBody(`["a"]`, `{}`)),
		"column not string":    string(rawBody(`[1]`, `[]`)),
		"columns not array":    string(rawBody(`"a"`, `[]`)),
		"huge exponent":        string(rawBody(`["a"]`, `[[1e999]]`)),
		"results not object":   `{"result":[{"results":[1,2],"success":true}],"success":true}`,
		"result not array":     `{"result":{"a":1},"success":true}`,
		"result item not obj":  `{"result":[1],"success":true}`,
		"top not object":       `[1,2]`,
		"truncated":            `{"result":[{"results":{"columns":["a"],"rows":[[1]`,
		"trailing data":        string(rawBody(`["a"]`, `[[1]]`)) + `{}`,
		"errors not array":     `{"errors":"x","success":false}`,
		"error message object": `{"errors":[{"code":1,"message":{"a":1}}],"success":false}`,
		"success not bool":     `{"success":"yes"}`,
		"too many results":     `{"result":[` + strings.Repeat(`{},`, maxResults) + `{}],"success":true}`,
	}
	for name, body := range bad {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s: panic %v", name, r)
				}
			}()
			if _, err := decodeResponse([]byte(body), defaultBudget()); err == nil {
				t.Errorf("%s: decoded without error", name)
			}
		}()
	}
}

func TestDecodeColumnsCapped(t *testing.T) {
	cols := func(n int) string {
		var b strings.Builder
		b.WriteByte('[')
		for i := range n {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`"c` + strconv.Itoa(i) + `"`)
		}
		b.WriteByte(']')
		return b.String()
	}
	if _, err := decodeResponse(rawBody(cols(maxColumns), `[]`), defaultBudget()); err != nil {
		t.Errorf("%d columns: %v", maxColumns, err)
	}
	if _, err := decodeResponse(rawBody(cols(maxColumns+1), `[]`), defaultBudget()); !errors.Is(err, driver.ErrLimit) {
		t.Errorf("%d columns: %v, want ErrLimit", maxColumns+1, err)
	}
}

func TestDecodeBudget(t *testing.T) {
	data := rawBody(`["a"]`, `[["`+strings.Repeat("x", 500)+`"],["`+strings.Repeat("y", 500)+`"]]`)
	if _, err := decodeResponse(data, newBudget(800, 100)); !errors.Is(err, driver.ErrLimit) {
		t.Errorf("text over the byte budget: %v, want ErrLimit", err)
	}
	if _, err := decodeResponse(data, newBudget(10000, 100)); err != nil {
		t.Errorf("within budget: %v", err)
	}
	if _, err := decodeResponse(rawBody(`["a"]`, `[[1],[2],[3]]`), newBudget(1<<20, 2)); !errors.Is(err, driver.ErrLimit) {
		t.Errorf("over the cell budget: %v, want ErrLimit", err)
	}
	blob := `[[[` + strings.Repeat("1,", 999) + `1]]]`
	if _, err := decodeResponse(rawBody(`["a"]`, blob), newBudget(900, 100)); !errors.Is(err, driver.ErrLimit) {
		t.Errorf("blob over the byte budget: %v, want ErrLimit", err)
	}
	// One budget covers every result of the response.
	two := []byte(`{"result":[{"results":{"columns":["a"],"rows":[["` + strings.Repeat("x", 600) + `"]]}},` +
		`{"results":{"columns":["a"],"rows":[["` + strings.Repeat("x", 600) + `"]]}}],"success":true}`)
	if _, err := decodeResponse(two, newBudget(1000, 100)); !errors.Is(err, driver.ErrLimit) {
		t.Errorf("results sharing the budget: %v, want ErrLimit", err)
	}
}

// allocDuring reports the peak growth of the live heap (HeapAlloc, sampled
// every 2 ms) while f runs.
func allocDuring(f func()) uint64 {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	base, peak := ms.HeapAlloc, ms.HeapAlloc
	done := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		tick := time.NewTicker(2 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				var m runtime.MemStats
				runtime.ReadMemStats(&m)
				peak = max(peak, m.HeapAlloc)
			}
		}
	}()
	f()
	close(done)
	<-sampled
	if peak < base {
		return 0
	}
	return peak - base
}

// TestHostileMemory: bodies built to amplify memory during decoding stay
// within about twice the decoding budget (which holds the result, plus
// garbage the GC has not collected yet) and end in an error, unless they
// are a legitimate result that fits.
func TestHostileMemory(t *testing.T) {
	const n = 3_000_000
	bodies := map[string]struct {
		body []byte
		ok   bool // a legitimate (if large) result that fits the budget
	}{
		"empty rows":   {body: rawBody(`[]`, "["+strings.Repeat("[],", n)+"[]]")},
		"many columns": {body: rawBody("["+strings.Repeat(`"",`, n)+`""]`, `[]`)},
		"null cells":   {body: rawBody(`["a"]`, "["+strings.Repeat("[null],", n)+"[null]]")},
		"objects":      {body: rawBody(`["a"]`, "[[{"+strings.Repeat(`"k":[],`, n)+`"k":[]}]]`)},
		"blob bytes":   {body: rawBody(`["a"]`, "[[["+strings.Repeat("1,", 4*n)+"1]]]"), ok: true},
	}
	for name, tc := range bodies {
		body := tc.body
		var err error
		alloc := allocDuring(func() { _, err = decodeResponse(body, defaultBudget()) })
		t.Logf("%s: peak heap growth %d MiB for a %d MiB body", name, alloc>>20, len(body)>>20)
		if (err == nil) != tc.ok {
			t.Errorf("%s: error %v, want ok=%v", name, err, tc.ok)
		}
		if limit := uint64(2*resultBudgetBytes + 4*len(body)); alloc > limit {
			t.Errorf("%s: allocated %d MiB for a %d MiB body", name, alloc>>20, len(body)>>20)
		}
	}
}
