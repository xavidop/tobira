package bigquery

import (
	"context"
	"errors"
	"fmt"
	"strings"

	bq "cloud.google.com/go/bigquery"

	"github.com/xavidop/tobira/driver"
)

var _ driver.Explainer = (*conn)(nil)

// Explain dry-runs the query: BigQuery validates it and estimates the bytes
// it would process without running it, so explaining never executes
// anything, writes included. The plan text lists the statement type, the
// byte estimate, the byte cap the run would carry, the result schema and
// the referenced tables. On a read-only call the text must pass the
// lexical allow-list first. BigQuery has no EXPLAIN ANALYZE (a finished
// job's stages are not exposed here), so Analyze is ErrUnsupported.
func (c *conn) Explain(ctx context.Context, q driver.Query, opts driver.ExplainOptions) (*driver.Plan, error) {
	const op = "explain"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	if c.readOnly || q.ReadOnly {
		if err := checkReadOnly(q.Text); err != nil {
			return nil, withOp(err, op)
		}
	}
	if opts.Analyze {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, errors.New("BigQuery has no EXPLAIN ANALYZE; run the query and read its job details instead"))
	}
	params, err := toParams(op, q.Params)
	if err != nil {
		return nil, err
	}
	st, err := c.dryRun(ctx, op, q, params)
	if err != nil {
		return nil, err
	}
	return &driver.Plan{Format: "text", Text: c.planText(st)}, nil
}

func (c *conn) planText(st *bq.QueryStatistics) string {
	var b strings.Builder
	typ := st.StatementType
	if typ == "" {
		typ = "(not reported)"
	}
	fmt.Fprintf(&b, "statement type: %s\n", typ)
	fmt.Fprintf(&b, "bytes processed (estimate): %d (%s)\n", st.TotalBytesProcessed, humanBytes(st.TotalBytesProcessed))
	if c.dsn.maxBytesBilled > 0 {
		fmt.Fprintf(&b, "maximum bytes billed: %d (%s)\n", c.dsn.maxBytesBilled, humanBytes(c.dsn.maxBytesBilled))
	} else {
		b.WriteString("maximum bytes billed: no cap\n")
	}
	if len(st.Schema) > 0 {
		b.WriteString("schema:\n")
		writeSchema(&b, st.Schema, "  ")
	} else {
		b.WriteString("schema: (not reported)\n")
	}
	if len(st.ReferencedTables) > 0 {
		b.WriteString("referenced tables:\n")
		for _, t := range st.ReferencedTables {
			fmt.Fprintf(&b, "  %s.%s.%s\n", t.ProjectID, t.DatasetID, t.TableID)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func writeSchema(b *strings.Builder, s bq.Schema, indent string) {
	for _, fs := range s {
		mode := ""
		switch {
		case fs.Repeated:
			mode = " REPEATED"
		case fs.Required:
			mode = " REQUIRED"
		}
		fmt.Fprintf(b, "%s%s %s%s\n", indent, fs.Name, fs.Type, mode)
		if fs.Type == bq.RecordFieldType {
			writeSchema(b, fs.Schema, indent+"  ")
		}
	}
}

// humanBytes renders a byte count in binary units: 2.0 KiB.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 5; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
