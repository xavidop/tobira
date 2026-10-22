package convert

import (
	"fmt"

	"github.com/xavidop/tobira/driver"
	pb "github.com/xavidop/tobira/plugin/proto/tobira/plugin/v1"
)

// checkNames enforces MaxNameBytes on every string of ss.
func checkNames(what string, ss []string) error {
	for i, s := range ss {
		if err := checkName(fmt.Sprintf("%s %d", what, i), s); err != nil {
			return err
		}
	}
	return nil
}

// strs returns ss, or nil when it is empty, so a decoded message compares
// equal to the driver value it came from.
func strs(ss []string) []string {
	if len(ss) == 0 {
		return nil
	}
	return ss
}

func typeFromProto(what string, t pb.ValueType) (driver.Type, error) {
	if t < pb.ValueType(driver.TypeNull) || t > pb.ValueType(driver.TypeUnknown) {
		return 0, invalidf("%s has unknown type %d", what, int32(t))
	}
	return driver.Type(t), nil
}

// InfoToProto encodes a driver's Info, every name under MaxNameBytes.
func InfoToProto(info driver.Info) (*pb.DriverInfo, error) {
	caps := make([]string, len(info.Capabilities))
	for i, c := range info.Capabilities {
		caps[i] = string(c)
	}
	p := &pb.DriverInfo{
		Name: info.Name, Kind: string(info.Kind), Schemes: info.Schemes,
		Dialect: info.Dialect, Version: info.Version, Capabilities: caps,
	}
	if err := checkInfo(p); err != nil {
		return nil, err
	}
	return p, nil
}

func checkInfo(p *pb.DriverInfo) error {
	for _, n := range []struct{ what, s string }{
		{"driver name", p.GetName()}, {"driver kind", p.GetKind()},
		{"dialect", p.GetDialect()}, {"driver version", p.GetVersion()},
	} {
		if err := checkName(n.what, n.s); err != nil {
			return err
		}
	}
	if err := checkNames("scheme", p.GetSchemes()); err != nil {
		return err
	}
	return checkNames("capability", p.GetCapabilities())
}

// InfoFromProto decodes a plugin's Info. A nil message is ErrInvalid.
func InfoFromProto(p *pb.DriverInfo) (driver.Info, error) {
	if p == nil {
		return driver.Info{}, invalidf("missing driver info")
	}
	if err := checkInfo(p); err != nil {
		return driver.Info{}, err
	}
	var caps []driver.Capability
	for _, c := range p.GetCapabilities() {
		caps = append(caps, driver.Capability(c))
	}
	return driver.Info{
		Name: p.GetName(), Kind: driver.Kind(p.GetKind()), Schemes: strs(p.GetSchemes()),
		Dialect: p.GetDialect(), Version: p.GetVersion(), Capabilities: caps,
	}, nil
}

// ConnConfigToProto encodes cfg, revealing its password: the message only
// ever travels over go-plugin's local channel. Embedder and Logger stay on
// the host.
func ConnConfigToProto(cfg driver.ConnConfig) *pb.ConnConfig {
	return &pb.ConnConfig{
		Name: cfg.Name, Driver: cfg.Driver, Dsn: cfg.DSN, Password: cfg.Password.Reveal(),
		ReadOnly: cfg.ReadOnly, Options: cfg.Options,
	}
}

// ConnConfigFromProto decodes a ConnConfig; nil decodes as the zero config.
// An empty password stays the zero Secret.
func ConnConfigFromProto(p *pb.ConnConfig) driver.ConnConfig {
	cfg := driver.ConnConfig{
		Name: p.GetName(), Driver: p.GetDriver(), DSN: p.GetDsn(),
		ReadOnly: p.GetReadOnly(), Options: p.GetOptions(),
	}
	if pw := p.GetPassword(); pw != "" {
		cfg.Password = driver.NewSecret(pw)
	}
	return cfg
}

// NamespacesToProto encodes a Namespaces result.
func NamespacesToProto(nss []driver.Namespace) ([]*pb.Namespace, error) {
	out := make([]*pb.Namespace, len(nss))
	for i, ns := range nss {
		p := &pb.Namespace{Name: ns.Name, Label: ns.Label, Default: ns.Default, Comment: ns.Comment}
		if err := checkNamespace(i, p); err != nil {
			return nil, err
		}
		out[i] = p
	}
	return out, nil
}

func checkNamespace(i int, p *pb.Namespace) error {
	if err := checkName(fmt.Sprintf("namespace %d name", i), p.GetName()); err != nil {
		return err
	}
	return checkName(fmt.Sprintf("namespace %d label", i), p.GetLabel())
}

// NamespacesFromProto decodes a Namespaces result; a nil entry is ErrInvalid.
func NamespacesFromProto(ps []*pb.Namespace) ([]driver.Namespace, error) {
	var out []driver.Namespace
	for i, p := range ps {
		if p == nil {
			return nil, invalidf("namespace %d missing", i)
		}
		if err := checkNamespace(i, p); err != nil {
			return nil, err
		}
		out = append(out, driver.Namespace{Name: p.GetName(), Label: p.GetLabel(), Default: p.GetDefault(), Comment: p.GetComment()})
	}
	return out, nil
}

// CollectionsToProto encodes a Collections result.
func CollectionsToProto(cs []driver.Collection) ([]*pb.Collection, error) {
	out := make([]*pb.Collection, len(cs))
	for i, c := range cs {
		if err := checkRef(c.Ref); err != nil {
			return nil, fmt.Errorf("collection %d: %w", i, err)
		}
		if err := checkName(fmt.Sprintf("collection %d type", i), string(c.Type)); err != nil {
			return nil, err
		}
		out[i] = &pb.Collection{Ref: CollectionRefToProto(c.Ref), Type: string(c.Type), RowEstimate: c.RowEstimate, Comment: c.Comment}
	}
	return out, nil
}

// CollectionsFromProto decodes a Collections result; a nil entry is
// ErrInvalid.
func CollectionsFromProto(ps []*pb.Collection) ([]driver.Collection, error) {
	var out []driver.Collection
	for i, p := range ps {
		if p == nil {
			return nil, invalidf("collection %d missing", i)
		}
		ref, err := CollectionRefFromProto(p.GetRef())
		if err != nil {
			return nil, fmt.Errorf("collection %d: %w", i, err)
		}
		if err := checkName(fmt.Sprintf("collection %d type", i), p.GetType()); err != nil {
			return nil, err
		}
		out = append(out, driver.Collection{Ref: ref, Type: driver.CollectionType(p.GetType()), RowEstimate: p.GetRowEstimate(), Comment: p.GetComment()})
	}
	return out, nil
}

// SchemaToProto encodes a Describe result. A nil schema is ErrInvalid.
func SchemaToProto(s *driver.CollectionSchema) (*pb.CollectionSchema, error) {
	if s == nil {
		return nil, invalidf("missing collection schema")
	}
	if err := checkRef(s.Ref); err != nil {
		return nil, err
	}
	fields, err := fieldsToProto(s.Fields, 0)
	if err != nil {
		return nil, err
	}
	p := &pb.CollectionSchema{
		Ref: CollectionRefToProto(s.Ref), Type: string(s.Type), Fields: fields,
		PrimaryKey: s.PrimaryKey, PartitionKey: s.PartitionKey, RowEstimate: s.RowEstimate, SizeBytes: s.SizeBytes,
		Schemaless: s.Schemaless, Comment: s.Comment, Ddl: s.DDL,
	}
	for _, k := range s.ClusteringKey {
		p.ClusteringKey = append(p.ClusteringKey, &pb.SortKey{Field: k.Field, Desc: k.Desc})
	}
	for _, ix := range s.Indexes {
		p.Indexes = append(p.Indexes, &pb.Index{Name: ix.Name, Fields: ix.Fields, Unique: ix.Unique, Primary: ix.Primary, Method: ix.Method})
	}
	for _, fk := range s.ForeignKeys {
		p.ForeignKeys = append(p.ForeignKeys, &pb.ForeignKey{
			Name: fk.Name, Fields: fk.Fields, References: CollectionRefToProto(fk.References), RefFields: fk.RefFields,
		})
	}
	for i, r := range s.Sample {
		pr, err := RowToProto(r)
		if err != nil {
			return nil, fmt.Errorf("sample row %d: %w", i, err)
		}
		p.Sample = append(p.Sample, pr)
	}
	return p, nil
}

func fieldsToProto(fs []driver.Field, depth int) ([]*pb.Field, error) {
	if len(fs) == 0 {
		return nil, nil
	}
	if depth >= MaxDepth {
		return nil, limitf("fields nest deeper than %d", MaxDepth)
	}
	out := make([]*pb.Field, len(fs))
	for i, f := range fs {
		if err := checkField(f.Name, f.NativeType); err != nil {
			return nil, err
		}
		nested, err := fieldsToProto(f.Fields, depth+1)
		if err != nil {
			return nil, err
		}
		out[i] = &pb.Field{
			Name: f.Name, NativeType: f.NativeType, Type: pb.ValueType(f.Type), Nullable: f.Nullable,
			Default: f.Default, Generated: f.Generated, AutoIncrement: f.AutoIncrement, Comment: f.Comment, Fields: nested,
		}
	}
	return out, nil
}

func checkField(name, native string) error {
	if err := checkName("field name", name); err != nil {
		return err
	}
	return checkName("field native type", native)
}

// SchemaFromProto decodes a Describe result: a nil schema, field, index,
// foreign key or sample row is ErrInvalid, names are capped at MaxNameBytes,
// fields nest at most MaxDepth levels and sample values are capped like any
// value.
func SchemaFromProto(p *pb.CollectionSchema) (*driver.CollectionSchema, error) {
	if p == nil {
		return nil, invalidf("missing collection schema")
	}
	ref, err := CollectionRefFromProto(p.GetRef())
	if err != nil {
		return nil, err
	}
	fields, err := fieldsFromProto(p.GetFields(), 0)
	if err != nil {
		return nil, err
	}
	if err := checkNames("primary key field", p.GetPrimaryKey()); err != nil {
		return nil, err
	}
	if err := checkNames("partition key field", p.GetPartitionKey()); err != nil {
		return nil, err
	}
	s := &driver.CollectionSchema{
		Ref: ref, Type: driver.CollectionType(p.GetType()), Fields: fields, PrimaryKey: strs(p.GetPrimaryKey()),
		PartitionKey: strs(p.GetPartitionKey()),
		RowEstimate:  p.GetRowEstimate(), SizeBytes: p.GetSizeBytes(), Schemaless: p.GetSchemaless(),
		Comment: p.GetComment(), DDL: p.GetDdl(),
	}
	for i, k := range p.GetClusteringKey() {
		if k == nil {
			return nil, invalidf("clustering key %d missing", i)
		}
		if err := checkName(fmt.Sprintf("clustering key field %d", i), k.GetField()); err != nil {
			return nil, err
		}
		s.ClusteringKey = append(s.ClusteringKey, driver.SortKey{Field: k.GetField(), Desc: k.GetDesc()})
	}
	for i, ix := range p.GetIndexes() {
		if ix == nil {
			return nil, invalidf("index %d missing", i)
		}
		if err := checkNames("index field", append([]string{ix.GetName()}, ix.GetFields()...)); err != nil {
			return nil, err
		}
		s.Indexes = append(s.Indexes, driver.Index{Name: ix.GetName(), Fields: strs(ix.GetFields()), Unique: ix.GetUnique(), Primary: ix.GetPrimary(), Method: ix.GetMethod()})
	}
	for i, fk := range p.GetForeignKeys() {
		if fk == nil {
			return nil, invalidf("foreign key %d missing", i)
		}
		to, err := CollectionRefFromProto(fk.GetReferences())
		if err != nil {
			return nil, fmt.Errorf("foreign key %d: %w", i, err)
		}
		if err := checkNames("foreign key field", append(append([]string{fk.GetName()}, fk.GetFields()...), fk.GetRefFields()...)); err != nil {
			return nil, err
		}
		s.ForeignKeys = append(s.ForeignKeys, driver.ForeignKey{Name: fk.GetName(), Fields: strs(fk.GetFields()), References: to, RefFields: strs(fk.GetRefFields())})
	}
	for i, pr := range p.GetSample() {
		r, err := RowFromProto(pr, len(pr.GetValues()))
		if err != nil {
			return nil, fmt.Errorf("sample row %d: %w", i, err)
		}
		s.Sample = append(s.Sample, r)
	}
	return s, nil
}

func fieldsFromProto(ps []*pb.Field, depth int) ([]driver.Field, error) {
	if len(ps) == 0 {
		return nil, nil
	}
	if depth >= MaxDepth {
		return nil, limitf("fields nest deeper than %d", MaxDepth)
	}
	out := make([]driver.Field, len(ps))
	for i, p := range ps {
		if p == nil {
			return nil, invalidf("field %d missing", i)
		}
		if err := checkField(p.GetName(), p.GetNativeType()); err != nil {
			return nil, err
		}
		t, err := typeFromProto(fmt.Sprintf("field %q", p.GetName()), p.GetType())
		if err != nil {
			return nil, err
		}
		nested, err := fieldsFromProto(p.GetFields(), depth+1)
		if err != nil {
			return nil, err
		}
		out[i] = driver.Field{
			Name: p.GetName(), NativeType: p.GetNativeType(), Type: t, Nullable: p.GetNullable(),
			Default: p.GetDefault(), Generated: p.GetGenerated(), AutoIncrement: p.GetAutoIncrement(),
			Comment: p.GetComment(), Fields: nested,
		}
	}
	return out, nil
}
