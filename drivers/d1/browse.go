package d1

import (
	"context"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/sqlitecore"
)

// Browse pages through a table or view with an optional filter and sort. It
// builds one SELECT (sqlitecore.BrowseQuery) and runs it through Query's
// checks, so the single-statement and read-only guards apply to the
// generated text (including Filter.Raw). The columns are typed from the
// Describe the query was built from.
func (c *conn) Browse(ctx context.Context, ref driver.CollectionRef, opts driver.BrowseOptions) (driver.ResultSet, error) {
	if err := c.check(ctx, "browse"); err != nil {
		return nil, err
	}
	page, err := sqlitecore.ParseBrowse(Name, opts)
	if err != nil {
		return nil, err
	}
	schema, err := c.Describe(ctx, ref)
	if err != nil {
		return nil, err
	}
	q, err := sqlitecore.BrowseQuery(Name, schema, opts, page)
	if err != nil {
		return nil, err
	}
	rs, err := c.query(ctx, q, schema)
	if err != nil {
		return nil, err
	}
	return page.Wrap(rs), nil
}
