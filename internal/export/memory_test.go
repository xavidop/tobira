package export_test

import (
	"context"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/drivertest"
	"github.com/xavidop/tobira/internal/export"
	"github.com/xavidop/tobira/internal/query"
)

// The memory tests sample the process-wide live heap, so no test in this
// package may call t.Parallel: a test running alongside would add its own
// live objects to the peak.

// heapBound is the most the live heap may grow above its pre-export
// baseline during an export, whatever the row count.
const heapBound = 16 << 20

// sampleEvery is how many generated rows pass between two synchronous
// samples taken by the source itself.
const sampleEvery = 5000

// liveHeap forces a full collection and returns the bytes still allocated:
// the live heap, not garbage waiting for a GC that is late under load.
func liveHeap() uint64 {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc
}

// peakHeapGrowth exports n generated rows in format f and returns the
// median, over three runs, of the peak live-heap growth during the export.
func peakHeapGrowth(t *testing.T, f query.Format, n int) uint64 {
	t.Helper()
	return peakHeapGrowthOf(t, func(sample func(int)) export.Source {
		fake := drivertest.NewFake()
		fake.OnQuery("gen", func(driver.Query) (driver.ResultSet, error) { return &genRS{n: n, onRow: sample}, nil })
		return export.QuerySource{Conn: open(t, fake), Query: driver.Query{Text: "gen"}}
	}, f, n)
}

// peakHeapGrowthOf exports n rows from the source mk builds, in format f,
// three times and returns the median peak live-heap growth. mk's source
// must call sample with each row's 1-based index.
//
// Each run measures the live heap after a forced GC, so GC pacing (a
// collection delayed by a loaded machine) cannot inflate it, while rows an
// exporter keeps reachable always count. The source itself samples every
// sampleEvery rows, synchronously: the exporter is blocked in Next while
// the GC runs, so it allocates nothing that a slow collection would count
// as live, and the samples land at the same rows whatever the scheduler
// does. Rows an exporter collects are reachable at the next sample.
func peakHeapGrowthOf(t *testing.T, mk func(sample func(int)) export.Source, f query.Format, n int) uint64 {
	t.Helper()
	runs := make([]uint64, 3)
	for i := range runs {
		runs[i] = onePeak(t, mk, f, n)
	}
	slices.Sort(runs)
	return runs[1]
}

func onePeak(t *testing.T, mk func(sample func(int)) export.Source, f query.Format, n int) uint64 {
	t.Helper()
	p := filepath.Join(t.TempDir(), "out")
	var peak atomic.Uint64
	var base uint64
	measure := func() {
		if live := liveHeap(); live > base {
			for g := live - base; ; {
				old := peak.Load()
				if g <= old || peak.CompareAndSwap(old, g) {
					break
				}
			}
		}
	}
	src := mk(func(i int) {
		if i%sampleEvery == 0 {
			measure()
		}
	})
	base = liveHeap()

	// The rewrite of a collection export (csv and json, when a page adds a
	// column) is sampled too: it runs after the last row, so the source's
	// samples cannot see what it holds.
	opts := export.WithRewriteSample(export.Options{Format: f, Path: p, NoNative: true}, measure)
	res, err := export.ToFile(context.Background(), src, opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.Rows != int64(n) {
		t.Fatalf("rows = %d, want %d", res.Rows, n)
	}
	return peak.Load()
}

// TestToFileConstantMemory streams 200k rows in every format with bounded
// heap growth, and shows the peak does not scale with the row count.
func TestToFileConstantMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("streams 200k rows per format")
	}
	for _, f := range []query.Format{query.FormatCSV, query.FormatJSON, query.FormatNDJSON} {
		t.Run(string(f), func(t *testing.T) {
			small := peakHeapGrowth(t, f, 10_000)
			large := peakHeapGrowth(t, f, 200_000)
			t.Logf("peak heap growth: 10k rows %d KiB, 200k rows %d KiB", small>>10, large>>10)
			if large > heapBound {
				t.Errorf("200k rows grew the heap by %d MiB, want < %d MiB", large>>20, heapBound>>20)
			}
			// 20x the rows must not mean meaningfully more memory: allow 1 MiB of
			// slack for buffers that size themselves to the data.
			if large > small+1<<20 {
				t.Errorf("peak grows with rows: 10k=%d KiB, 200k=%d KiB", small>>10, large>>10)
			}
		})
	}
}

// unionPages serves n generated rows in pages of 1000 whose columns grow
// after the first page, so csv and json take the rewrite path. sample gets
// each row's 1-based index across pages.
func unionPages(t *testing.T, n int, sample func(int)) driver.Conn {
	t.Helper()
	const pageSize = 1000
	wide := append(slices.Clone(genCols), driver.Column{Name: "extra", Type: driver.TypeString})
	inner := open(t, drivertest.NewFake())
	return &browseConn{Conn: inner, browse: func(_ context.Context, _ driver.CollectionRef, o driver.BrowseOptions) (driver.ResultSet, error) {
		page := 0
		if o.Cursor != "" {
			page, _ = strconv.Atoi(o.Cursor)
		}
		rows := min(pageSize, n-page*pageSize)
		next := ""
		if (page+1)*pageSize < n {
			next = strconv.Itoa(page + 1)
		}
		cols := genCols
		if page > 0 {
			cols = wide
		}
		first := page * pageSize
		return &pageRS{gen: genRS{n: rows, onRow: func(i int) { sample(first + i) }}, cols: cols, next: next}, nil
	}}
}

// pageRS is one generated page; rows of a wide page get an extra value.
type pageRS struct {
	gen  genRS
	cols []driver.Column
	next string
}

func (p *pageRS) Columns() []driver.Column { return p.cols }
func (p *pageRS) Next(ctx context.Context) (driver.Row, bool, error) {
	row, ok, err := p.gen.Next(ctx)
	if ok && len(p.cols) > len(row) {
		row = append(row, driver.String("extra"))
	}
	return row, ok, err
}
func (p *pageRS) Stats() driver.Stats { return driver.Stats{RowsAffected: -1, NextCursor: p.next} }
func (p *pageRS) Close() error        { return nil }

// TestToFileCollectionUnionConstantMemory shows the union rewrite keeps
// memory constant: columns grow after page 1, so csv and json are rewritten.
func TestToFileCollectionUnionConstantMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("streams 200k rows per format")
	}
	for _, f := range []query.Format{query.FormatCSV, query.FormatJSON, query.FormatNDJSON} {
		t.Run(string(f), func(t *testing.T) {
			union := func(n int) func(func(int)) export.Source {
				return func(sample func(int)) export.Source {
					return export.CollectionSource{Conn: unionPages(t, n, sample), Ref: peopleRef}
				}
			}
			small := peakHeapGrowthOf(t, union(10_000), f, 10_000)
			large := peakHeapGrowthOf(t, union(200_000), f, 200_000)
			t.Logf("peak heap growth: 10k rows %d KiB, 200k rows %d KiB", small>>10, large>>10)
			if large > heapBound {
				t.Errorf("200k rows grew the heap by %d MiB, want < %d MiB", large>>20, heapBound>>20)
			}
			if large > small+1<<20 {
				t.Errorf("peak grows with rows: 10k=%d KiB, 200k=%d KiB", small>>10, large>>10)
			}
		})
	}
}
