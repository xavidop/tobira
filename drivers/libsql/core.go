package libsql

import (
	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/sqlitecore"
)

// The SQLite dialect logic lives in driverutil/sqlitecore, shared with the
// sqlite and d1 drivers; these bind it to this driver's name.
// TestGuardCorpusMatchesSQLite pins every guard decision to the sqlite
// driver's corpus.

func checkReadOnly(text string) error { return sqlitecore.CheckReadOnly(Name, text) }
func checkSingle(text string) error   { return sqlitecore.CheckSingle(Name, text) }
func checkExplain(text string) error  { return sqlitecore.CheckExplain(Name, text) }
func returnsRows(text string) bool    { return sqlitecore.ReturnsRows(text) }
func isTxControl(text string) bool    { return sqlitecore.IsTxControl(text) }

// readOnlyGuard is the read-only path's lexical gate: the sqlite driver's
// allow-list, then sqlitecore.CheckNoWrite. The sqlite driver lets
// "WITH … DELETE" through its allow-list because PRAGMA query_only refuses
// it on the server; sqld refuses PRAGMA query_only, so this driver refuses
// the writable CTE itself (BEGIN … ROLLBACK would still undo it), and
// load_extension, which the rollback would not undo.
func readOnlyGuard(text string) error { return sqlitecore.ReadOnlyGuard(Name, text) }

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

func column(op string, schema *driver.CollectionSchema, name string) (string, error) {
	return sqlitecore.Column(Name, op, schema, name)
}

func whereKey(op string, schema *driver.CollectionSchema, key driver.Object) (string, []driver.Value, error) {
	return sqlitecore.WhereKey(Name, op, schema, key)
}
