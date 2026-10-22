package mongodb

import (
	"errors"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/xavidop/tobira/driver"
)

// readOnlyCommands is the read-only allow-list. Command names are matched
// exactly (MongoDB's own lookup is case-sensitive), so a spelling the list
// does not hold is refused even when the server has an alias for it.
var readOnlyCommands = map[string]bool{
	"find": true, "aggregate": true, "count": true, "distinct": true,
	"listCollections": true, "listIndexes": true, "collStats": true, "dbStats": true,
	"explain": true,
}

// explainable are the commands explain may wrap on a read-only call.
var explainable = map[string]bool{"find": true, "aggregate": true, "count": true, "distinct": true}

// writeStages are the aggregation stages that write.
var writeStages = map[string]bool{"$out": true, "$merge": true}

// readOnlyReadConcerns are the read concern levels a read-only call may
// name. linearizable is refused: each such read makes the primary write a
// no-op oplog entry.
var readOnlyReadConcerns = map[string]bool{"local": true, "available": true, "majority": true, "snapshot": true}

// refusedKeys are top-level fields a read-only command may not carry: the
// session and transaction fields, and writeConcern, which only a write
// needs.
var refusedKeys = map[string]bool{
	"lsid": true, "txnNumber": true, "startTransaction": true, "autocommit": true, "writeConcern": true,
}

// checkReadOnly is the read-only guard. It inspects the parsed command
// document, which is exactly what is sent to the server, and fails closed:
//
//   - the command (the first key) must be on the allow-list: find,
//     aggregate, count, distinct, listCollections, listIndexes, collStats,
//     dbStats and explain;
//   - explain must wrap a document whose command is find, aggregate, count
//     or distinct, itself checked the same way;
//   - no key may appear twice at the top level, and no top-level key may
//     start with "$" or be a session, transaction or write-concern field;
//   - a readConcern must be a document with level local, available,
//     majority or snapshot (linearizable makes the primary write a no-op
//     oplog entry);
//   - no key named $out or $merge may appear anywhere in the document, at
//     any depth: in a pipeline, a $facet, a $lookup or $unionWith
//     sub-pipeline, or anywhere else.
//
// Everything else, including every write command, mapReduce and eval, is
// ErrReadOnly. Server-side JavaScript ($where, $function, $accumulator) is
// allowed: it cannot write.
func checkReadOnly(cmd bson.D) error {
	return checkCommand(cmd, false)
}

func checkCommand(cmd bson.D, inExplain bool) error {
	if len(cmd) == 0 {
		return refuse(errors.New("empty command document"))
	}
	name := cmd[0].Key
	allowed := readOnlyCommands[name]
	if inExplain {
		allowed = explainable[name]
	}
	if !allowed {
		return refuse(fmt.Errorf("command %q is not allowed on a read-only call", name))
	}
	seen := make(map[string]bool, len(cmd))
	for _, e := range cmd {
		if seen[e.Key] {
			return refuse(fmt.Errorf("field %q appears twice", e.Key))
		}
		seen[e.Key] = true
		if e.Key != name && (len(e.Key) > 0 && e.Key[0] == '$' || refusedKeys[e.Key]) {
			return refuse(fmt.Errorf("field %q is not allowed on a read-only call", e.Key))
		}
	}
	if err := checkReadConcern(cmd); err != nil {
		return err
	}
	if err := checkNoWriteStage(cmd); err != nil {
		return err
	}
	if name == "explain" {
		inner, ok := cmd[0].Value.(bson.D)
		if !ok {
			return refuse(errors.New("explain must wrap a command document"))
		}
		return checkCommand(inner, true)
	}
	return nil
}

// checkReadConcern allows a readConcern only as a document whose level, if
// any, is local, available, majority or snapshot.
func checkReadConcern(cmd bson.D) error {
	rc, ok := lookup(cmd, "readConcern")
	if !ok {
		return nil
	}
	d, ok := rc.(bson.D)
	if !ok {
		return refuse(errors.New("readConcern must be a document"))
	}
	if lv, ok := lookup(d, "level"); ok {
		s, _ := lv.(string)
		if !readOnlyReadConcerns[s] {
			return refuse(fmt.Errorf("read concern level %v is not allowed on a read-only call: use local, available, majority or snapshot", lv))
		}
	}
	return nil
}

// checkNoWriteStage refuses a value holding a key named $out or $merge at
// any depth.
func checkNoWriteStage(v any) error {
	switch x := v.(type) {
	case bson.D:
		for _, e := range x {
			if writeStages[e.Key] {
				return refuse(fmt.Errorf("%s writes: it is not allowed on a read-only call", e.Key))
			}
			if err := checkNoWriteStage(e.Value); err != nil {
				return err
			}
		}
	case bson.A:
		for _, e := range x {
			if err := checkNoWriteStage(e); err != nil {
				return err
			}
		}
	case bson.M:
		for k, e := range x {
			if writeStages[k] {
				return refuse(fmt.Errorf("%s writes: it is not allowed on a read-only call", k))
			}
			if err := checkNoWriteStage(e); err != nil {
				return err
			}
		}
	}
	return nil
}

func refuse(err error) error {
	return driver.Wrap(driver.ErrReadOnly, Name, "query", err)
}
