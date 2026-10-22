package sqlite

import (
	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/sqlitecore"
)

// The SQLite dialect logic lives in driverutil/sqlitecore, shared with the
// libsql and d1 drivers; these bind it to this driver's name.

func checkReadOnly(text string) error   { return sqlitecore.CheckReadOnly(Name, text) }
func checkSingle(text string) error     { return sqlitecore.CheckSingle(Name, text) }
func checkExplain(text string) error    { return sqlitecore.CheckExplain(Name, text) }
func returnsRows(text string) bool      { return sqlitecore.ReturnsRows(text) }
func isTxControl(text string) bool      { return sqlitecore.IsTxControl(text) }
func isWriteStatement(text string) bool { return sqlitecore.IsWriteStatement(text) }

func quoteIdent(s string) string { return sqlitecore.QuoteIdent(s) }
func nsName(n string) string     { return sqlitecore.NamespaceName(n) }

func mapType(declared string) driver.Type { return sqlitecore.MapType(declared) }
func columnType(declared string, sample any) driver.Type {
	return sqlitecore.ColumnType(declared, sample)
}
func convertValue(declared string, v any) driver.Value { return sqlitecore.ConvertValue(declared, v) }
func convertTyped(declared string, typ driver.Type, v any) driver.Value {
	return sqlitecore.ConvertTyped(declared, typ, v)
}

func markRowidAlias(s *driver.CollectionSchema)            { sqlitecore.MarkRowidAlias(s) }
func rowidTiebreak(schema *driver.CollectionSchema) string { return sqlitecore.RowidTiebreak(schema) }

func column(op string, schema *driver.CollectionSchema, name string) (string, error) {
	return sqlitecore.Column(Name, op, schema, name)
}

func whereKey(op string, schema *driver.CollectionSchema, key driver.Object) (string, []driver.Value, error) {
	return sqlitecore.WhereKey(Name, op, schema, key)
}
