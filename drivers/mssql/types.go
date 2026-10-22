package mssql

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	mssqldb "github.com/microsoft/go-mssqldb"

	"github.com/xavidop/tobira/driver"
)

// columnType maps a result column's type, as go-mssqldb names it
// (ColumnType.DatabaseTypeName), to tobira's Type. TIME is a time of day
// with no date, so it stays String; a UNIQUEIDENTIFIER is its canonical
// text. CLR types (GEOGRAPHY, GEOMETRY, HIERARCHYID) are their raw bytes.
// SQL_VARIANT is Unknown: each value is typed by what it holds.
func columnType(dbType string) driver.Type {
	switch dbType {
	case "BIT":
		return driver.TypeBool
	case "TINYINT", "SMALLINT", "INT", "BIGINT":
		return driver.TypeInt
	case "DECIMAL", "NUMERIC", "MONEY", "SMALLMONEY":
		return driver.TypeDecimal
	case "FLOAT", "REAL":
		return driver.TypeFloat
	case "DATE", "DATETIME", "DATETIME2", "SMALLDATETIME", "DATETIMEOFFSET":
		return driver.TypeTime
	case "TIME", "CHAR", "VARCHAR", "NCHAR", "NVARCHAR", "TEXT", "NTEXT", "XML", "UNIQUEIDENTIFIER":
		return driver.TypeString
	case "BINARY", "VARBINARY", "IMAGE", "GEOGRAPHY", "GEOMETRY", "HIERARCHYID":
		return driver.TypeBytes
	case "JSON":
		return driver.TypeJSON
	}
	return driver.TypeUnknown
}

// fieldType maps a sys.types name (the base type of an alias type) to
// tobira's Type.
func fieldType(typeName string) driver.Type {
	switch t := strings.ToLower(typeName); t {
	case "sysname":
		return driver.TypeString
	case "timestamp", "rowversion":
		return driver.TypeBytes
	default:
		return columnType(strings.ToUpper(t))
	}
}

// nativeType renders a column's type as SQL Server spells it in DDL, from
// sys.columns: max_length is in bytes (-1 for max), so the n… types halve
// it.
func nativeType(name string, maxLen, precision, scale int) string {
	switch strings.ToLower(name) {
	case "varchar", "char", "varbinary", "binary":
		if maxLen < 0 {
			return name + "(max)"
		}
		return fmt.Sprintf("%s(%d)", name, maxLen)
	case "nvarchar", "nchar":
		if maxLen < 0 {
			return name + "(max)"
		}
		return fmt.Sprintf("%s(%d)", name, maxLen/2)
	case "decimal", "numeric":
		return fmt.Sprintf("%s(%d,%d)", name, precision, scale)
	case "datetime2", "datetimeoffset", "time":
		return fmt.Sprintf("%s(%d)", name, scale)
	case "float":
		return fmt.Sprintf("%s(%d)", name, precision)
	}
	return name
}

// convertValue converts a value scanned from a column of type dbType.
// go-mssqldb returns integers as int64, REAL and FLOAT as float64, BIT as
// bool, DECIMAL and MONEY as their decimal text in bytes, dates and times as
// time.Time (decoded in UTC), and GUIDs as their 16 wire bytes.
func convertValue(dbType string, v any) driver.Value {
	switch x := v.(type) {
	case nil:
		return driver.Null()
	case int64:
		return driver.Int(x)
	case float64:
		if dbType == "REAL" {
			// Keep the shortest decimal form of the float32 (0.1, not 0.10000000149).
			f, _ := strconv.ParseFloat(strconv.FormatFloat(x, 'g', -1, 32), 64)
			return driver.Float(f)
		}
		return driver.Float(x)
	case bool:
		return driver.Bool(x)
	case time.Time:
		if dbType == "TIME" {
			return driver.String(x.Format("15:04:05.9999999"))
		}
		return driver.Time(x.UTC())
	case string:
		return driver.String(x)
	case []byte:
		switch dbType {
		case "DECIMAL", "NUMERIC", "MONEY", "SMALLMONEY":
			return driver.Decimal(string(x))
		case "UNIQUEIDENTIFIER":
			var u mssqldb.UniqueIdentifier
			if err := u.Scan(x); err == nil {
				return driver.String(u.String())
			}
		}
		return driver.Bytes(x)
	}
	return driver.Unknown(dbType, nil, fmt.Sprint(v))
}

var errUnsupportedParam = errors.New("this value type cannot be a SQL Server parameter")

// toArg converts a query parameter to what go-mssqldb binds. Decimals travel
// as text so they stay exact (the server converts them to the column's
// type), JSON as its text, and times in UTC.
func toArg(v driver.Value) (any, error) {
	switch v.Type() {
	case driver.TypeNull:
		return nil, nil
	case driver.TypeBool:
		b, _ := v.AsBool()
		return b, nil
	case driver.TypeInt:
		i, _ := v.AsInt()
		return i, nil
	case driver.TypeFloat:
		f, _ := v.AsFloat()
		return f, nil
	case driver.TypeDecimal, driver.TypeString:
		s, _ := v.AsText()
		return s, nil
	case driver.TypeBytes:
		b, _ := v.AsBytes()
		return b, nil
	case driver.TypeTime:
		t, _ := v.AsTime()
		return t.UTC(), nil
	case driver.TypeJSON:
		raw, _ := v.AsJSON()
		return string(raw), nil
	}
	return nil, fmt.Errorf("%w: %s", errUnsupportedParam, v.Type())
}
