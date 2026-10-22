package driver_test

import (
	"context"
	"io"

	"github.com/xavidop/tobira/driver"
)

// everything implements every optional interface.
type everything struct{ baseConn }

func (*everything) Browse(context.Context, driver.CollectionRef, driver.BrowseOptions) (driver.ResultSet, error) {
	return nil, nil
}
func (*everything) Insert(context.Context, driver.CollectionRef, driver.Object) (driver.Object, error) {
	return nil, nil
}
func (*everything) Update(context.Context, driver.CollectionRef, driver.Object, driver.Object) error {
	return nil
}
func (*everything) Delete(context.Context, driver.CollectionRef, driver.Object) error { return nil }
func (*everything) Explain(context.Context, driver.Query, driver.ExplainOptions) (*driver.Plan, error) {
	return nil, nil
}
func (*everything) Begin(context.Context, driver.TxOptions) (driver.Tx, error) { return nil, nil }
func (*everything) Similar(context.Context, driver.CollectionRef, driver.VectorQuery) (driver.ResultSet, error) {
	return nil, nil
}
func (*everything) Range(context.Context, driver.CollectionRef, driver.RangeQuery) (driver.ResultSet, error) {
	return nil, nil
}
func (*everything) Neighbors(context.Context, driver.GraphQuery) (*driver.Graph, error) {
	return nil, nil
}
func (*everything) Complete(context.Context, driver.CompletionRequest) ([]driver.Completion, error) {
	return nil, nil
}
func (*everything) ExportFormats() []string { return nil }
func (*everything) Export(context.Context, driver.Query, string, io.Writer) error {
	return nil
}
func (*everything) Watch(context.Context, driver.WatchRequest) (<-chan driver.Event, error) {
	return nil, nil
}
