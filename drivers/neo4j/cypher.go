package neo4j

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xavidop/tobira/driver"
)

// idColumn is the column that carries a node's element id in Browse and
// Describe: the key of a label, since nodes have no other one.
const idColumn = "_id"

// quote backticks a label, property or database name for Cypher, doubling
// backticks inside it.
func quote(name string) string { return "`" + strings.ReplaceAll(name, "`", "``") + "`" }

// paramName is the name positional parameter i (1-based) binds to: Cypher
// accepts $1, $2 … as parameter names.
func paramName(i int) string { return strconv.Itoa(i) }

// params lays Query.Params out as the Go driver wants them: "1", "2" … so
// the query refers to them as $1, $2 ….
func params(vals []driver.Value) (map[string]any, error) {
	if len(vals) == 0 {
		return nil, nil
	}
	out := make(map[string]any, len(vals))
	for i, v := range vals {
		p, err := encode(v)
		if err != nil {
			return nil, fmt.Errorf("parameter %d: %w", i+1, err)
		}
		out[paramName(i+1)] = p
	}
	return out, nil
}

// condition renders one Browse condition over node variable n as a Cypher
// predicate, binding its value to the next parameter. prop must be a known
// property of the label, or the id column.
func condition(n string, c driver.Condition, bind func(any) string) (string, error) {
	field := n + "." + quote(c.Field)
	if c.Field == idColumn {
		field = "elementId(" + n + ")"
	}
	switch c.Op {
	case driver.OpIsNull:
		return field + " IS NULL", nil
	case driver.OpNotNull:
		return field + " IS NOT NULL", nil
	}
	v, err := encode(c.Value)
	if err != nil {
		return "", err
	}
	switch c.Op {
	case driver.OpEq:
		return field + " = " + bind(v), nil
	case driver.OpNe:
		return field + " <> " + bind(v), nil
	case driver.OpLt:
		return field + " < " + bind(v), nil
	case driver.OpLe:
		return field + " <= " + bind(v), nil
	case driver.OpGt:
		return field + " > " + bind(v), nil
	case driver.OpGe:
		return field + " >= " + bind(v), nil
	case driver.OpIn:
		if c.Value.Type() != driver.TypeList {
			return "", errors.New("in needs a list value")
		}
		return field + " IN " + bind(v), nil
	case driver.OpContains:
		return "toString(" + field + ") CONTAINS toString(" + bind(v) + ")", nil
	case driver.OpPrefix:
		return "toString(" + field + ") STARTS WITH toString(" + bind(v) + ")", nil
	}
	return "", fmt.Errorf("unsupported operator %q", c.Op)
}

// schemaCache keeps Describe answers for a short while: Browse asks for
// the label's properties on every page, and db.schema.nodeTypeProperties
// reads the whole graph.
type schemaCache struct {
	mu   sync.Mutex
	ttl  time.Duration
	now  func() time.Time
	rows map[driver.CollectionRef]cachedSchema
}

type cachedSchema struct {
	sch *driver.CollectionSchema
	at  time.Time
}

const schemaTTL = 30 * time.Second

func newSchemaCache() *schemaCache {
	return &schemaCache{ttl: schemaTTL, now: time.Now, rows: map[driver.CollectionRef]cachedSchema{}}
}

func (s *schemaCache) get(ref driver.CollectionRef) (*driver.CollectionSchema, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.rows[ref]
	if !ok || s.now().Sub(e.at) > s.ttl {
		delete(s.rows, ref)
		return nil, false
	}
	return e.sch, true
}

func (s *schemaCache) put(ref driver.CollectionRef, sch *driver.CollectionSchema) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[ref] = cachedSchema{sch: sch, at: s.now()}
}

// Invalidate forgets every cached schema.
func (s *schemaCache) Invalidate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.rows)
}
