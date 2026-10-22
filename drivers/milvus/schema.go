package milvus

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/milvus-io/milvus-proto/go-api/v2/milvuspb"
	"github.com/milvus-io/milvus-proto/go-api/v2/schemapb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/xavidop/tobira/driver"
)

// Column names the driver adds to result rows, and the prefix of a dynamic
// key whose column would clash with a field or one of them.
const (
	scoreColumn    = "_score"
	distanceColumn = "_distance"
	dynamicPrefix  = "$meta."
	dynamicField   = "$meta"
)

// fieldInfo describes one field of a collection schema.
type fieldInfo struct {
	name        string
	dataType    schemapb.DataType
	elemType    schemapb.DataType // arrays
	dim         int               // dense vectors
	maxLen      int               // varchar
	maxCap      int               // arrays
	nullable    bool
	pk          bool
	autoID      bool
	hasDefault  bool
	defaultText string
	description string
	// output is the function producing this field ("" = none).
	output string
}

// isVector reports whether the field holds a vector.
func (f *fieldInfo) isVector() bool {
	switch f.dataType {
	case schemapb.DataType_FloatVector, schemapb.DataType_Float16Vector, schemapb.DataType_BFloat16Vector,
		schemapb.DataType_BinaryVector, schemapb.DataType_SparseFloatVector, schemapb.DataType_Int8Vector:
		return true
	}
	return false
}

// driverType is the tobira type of the field's values.
func (f *fieldInfo) driverType() driver.Type {
	switch f.dataType {
	case schemapb.DataType_Bool:
		return driver.TypeBool
	case schemapb.DataType_Int8, schemapb.DataType_Int16, schemapb.DataType_Int32, schemapb.DataType_Int64:
		return driver.TypeInt
	case schemapb.DataType_Float, schemapb.DataType_Double:
		return driver.TypeFloat
	case schemapb.DataType_String, schemapb.DataType_VarChar, schemapb.DataType_Geometry:
		return driver.TypeString
	case schemapb.DataType_JSON:
		return driver.TypeJSON
	case schemapb.DataType_Timestamptz:
		return driver.TypeTime
	case schemapb.DataType_Array, schemapb.DataType_Int8Vector, schemapb.DataType_ArrayOfStruct, schemapb.DataType_ArrayOfVector:
		return driver.TypeList
	case schemapb.DataType_FloatVector, schemapb.DataType_Float16Vector, schemapb.DataType_BFloat16Vector:
		return driver.TypeVector
	case schemapb.DataType_BinaryVector:
		return driver.TypeBytes
	case schemapb.DataType_SparseFloatVector:
		return driver.TypeObject
	}
	return driver.TypeUnknown
}

// nativeType spells the field's type as Milvus does: "VarChar(64)",
// "FloatVector(4)", "Array<Int64>(16)".
func (f *fieldInfo) nativeType() string {
	t := f.dataType.String()
	switch {
	case f.dataType == schemapb.DataType_VarChar && f.maxLen > 0:
		return fmt.Sprintf("%s(%d)", t, f.maxLen)
	case f.dataType == schemapb.DataType_Array:
		return fmt.Sprintf("Array<%s>(%d)", f.elemType, f.maxCap)
	case f.dim > 0:
		return fmt.Sprintf("%s(%d)", t, f.dim)
	}
	return t
}

// functionInfo is a schema function: BM25 or TEXTEMBEDDING.
type functionInfo struct {
	name    string
	typ     schemapb.FunctionType
	inputs  []string
	outputs []string
}

// collSchema is what the driver needs to know about a collection.
type collSchema struct {
	db, name    string
	fields      []fieldInfo // static fields in schema order, without $meta
	dynamic     bool
	functions   []functionInfo
	description string
	raw         *schemapb.CollectionSchema
	updateTS    uint64
}

// getSchema describes a collection. A missing collection is ErrNotFound.
func (c *conn) getSchema(ctx context.Context, op, db, name string) (*collSchema, error) {
	if name == "" {
		return nil, driver.Wrap(driver.ErrSyntax, Name, op, fmt.Errorf("no collection named"))
	}
	resp, err := c.svc.DescribeCollection(ctx, &milvuspb.DescribeCollectionRequest{DbName: db, CollectionName: name})
	if err := checkRPC(resp, err); err != nil {
		return nil, mapError(ctx, op, err)
	}
	return schemaFromProto(db, resp), nil
}

// schemaFromProto builds a collSchema from a DescribeCollection reply.
func schemaFromProto(db string, resp *milvuspb.DescribeCollectionResponse) *collSchema {
	name := resp.GetCollectionName()
	if name == "" {
		name = resp.GetSchema().GetName()
	}
	s := schemaFromProtoSchema(db, name, resp.GetSchema())
	s.updateTS = resp.GetUpdateTimestamp()
	return s
}

// schemaFromProtoSchema builds a collSchema from a collection schema.
func schemaFromProtoSchema(db, name string, p *schemapb.CollectionSchema) *collSchema {
	s := &collSchema{db: db, name: name, dynamic: p.GetEnableDynamicField(), description: p.GetDescription(), raw: p}
	outputOf := map[string]string{}
	for _, fn := range p.GetFunctions() {
		s.functions = append(s.functions, functionInfo{name: fn.GetName(), typ: fn.GetType(), inputs: fn.GetInputFieldNames(), outputs: fn.GetOutputFieldNames()})
		for _, o := range fn.GetOutputFieldNames() {
			outputOf[o] = fn.GetName()
		}
	}
	for _, f := range p.GetFields() {
		if f.GetIsDynamic() {
			s.dynamic = true
			continue
		}
		fi := fieldInfo{
			name:        f.GetName(),
			dataType:    f.GetDataType(),
			elemType:    f.GetElementType(),
			nullable:    f.GetNullable(),
			pk:          f.GetIsPrimaryKey(),
			autoID:      f.GetAutoID(),
			description: f.GetDescription(),
			output:      outputOf[f.GetName()],
		}
		for _, kv := range f.GetTypeParams() {
			n, _ := strconv.Atoi(kv.GetValue())
			switch kv.GetKey() {
			case "dim":
				fi.dim = n
			case "max_length":
				fi.maxLen = n
			case "max_capacity":
				fi.maxCap = n
			}
		}
		if dv := f.GetDefaultValue(); dv != nil && dv.GetData() != nil {
			fi.hasDefault = true
			fi.defaultText = defaultText(dv)
		}
		s.fields = append(s.fields, fi)
	}
	return s
}

// defaultText spells a field's default value.
func defaultText(dv *schemapb.ValueField) string {
	switch d := dv.GetData().(type) {
	case *schemapb.ValueField_BoolData:
		return strconv.FormatBool(d.BoolData)
	case *schemapb.ValueField_IntData:
		return strconv.FormatInt(int64(d.IntData), 10)
	case *schemapb.ValueField_LongData:
		return strconv.FormatInt(d.LongData, 10)
	case *schemapb.ValueField_FloatData:
		return strconv.FormatFloat(float64(d.FloatData), 'g', -1, 32)
	case *schemapb.ValueField_DoubleData:
		return strconv.FormatFloat(d.DoubleData, 'g', -1, 64)
	case *schemapb.ValueField_StringData:
		return strconv.Quote(d.StringData)
	case *schemapb.ValueField_TimestamptzData:
		return strconv.FormatInt(d.TimestamptzData, 10)
	}
	return ""
}

// field returns the static field named name.
func (s *collSchema) field(name string) (*fieldInfo, bool) {
	for i := range s.fields {
		if s.fields[i].name == name {
			return &s.fields[i], true
		}
	}
	return nil, false
}

// pkField returns the primary key field. Every Milvus collection has one.
func (s *collSchema) pkField() *fieldInfo {
	for i := range s.fields {
		if s.fields[i].pk {
			return &s.fields[i]
		}
	}
	return &fieldInfo{name: "id", dataType: schemapb.DataType_Int64, pk: true}
}

// function returns the function producing field f, or nil.
func (s *collSchema) function(f *fieldInfo) *functionInfo {
	if f.output == "" {
		return nil
	}
	for i := range s.functions {
		if s.functions[i].name == f.output {
			return &s.functions[i]
		}
	}
	return nil
}

// columnName is the column of dynamic key: the key itself, or the key
// with one more "$meta." prefix when it clashes with a static field, a
// column the driver adds (_score, _distance) or already starts with the
// prefix.
func (s *collSchema) columnName(key string) string {
	if _, clash := s.field(key); clash || key == scoreColumn || key == distanceColumn || strings.HasPrefix(key, dynamicPrefix) {
		return dynamicPrefix + key
	}
	return key
}

// dynamicKey is the inverse of columnName: the dynamic key a column
// names, or false for a static field.
func (s *collSchema) dynamicKey(column string) (string, bool) {
	if _, static := s.field(column); static {
		return "", false
	}
	if strings.HasPrefix(column, dynamicPrefix) {
		return column[len(dynamicPrefix):], true
	}
	return column, true
}

// secretParam reports whether a function parameter may hold a credential
// (older TEXTEMBEDDING definitions took the provider's api_key inline).
func secretParam(key string) bool {
	k := strings.ToLower(key)
	for _, s := range []string{"key", "secret", "token", "password", "credential", "auth"} {
		if strings.Contains(k, s) {
			return true
		}
	}
	return false
}

// ddl is the collection schema as JSON, with function parameters that may
// hold credentials masked.
func (s *collSchema) ddl() string {
	if s.raw == nil {
		return ""
	}
	p := proto.Clone(s.raw).(*schemapb.CollectionSchema)
	for _, fn := range p.GetFunctions() {
		for _, kv := range fn.GetParams() {
			if secretParam(kv.GetKey()) {
				kv.Value = "xxxxx"
			} else {
				kv.Value = maskURL(kv.GetValue())
			}
		}
	}
	b, err := protojson.MarshalOptions{Multiline: true, UseProtoNames: true}.Marshal(p)
	if err != nil {
		return ""
	}
	return string(b)
}

// maskURL masks the credentials a URL-shaped param may carry: its
// userinfo and its query values. Anything else is returned as is.
func maskURL(v string) string {
	if !strings.Contains(v, "://") {
		return v
	}
	u, err := url.Parse(v)
	if err != nil {
		return "xxxxx" // unparseable: it may hold anything
	}
	if u.User != nil {
		u.User = url.User("xxxxx")
	}
	if u.RawQuery != "" {
		q := u.Query()
		for k := range q {
			q[k] = []string{"xxxxx"}
		}
		u.RawQuery = q.Encode()
	}
	return u.String()
}
