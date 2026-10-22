package mongodb

import (
	"errors"
	"testing"

	"github.com/xavidop/tobira/driver"
)

// readOnlyAllowed are commands the read-only guard lets through.
var readOnlyAllowed = []string{
	`{"find":"c","filter":{"a":1},"sort":{"_id":1},"limit":5}`,
	`{"find":"c","filter":{"$where":"sleep(1) || true"}}`,
	`{"aggregate":"c","pipeline":[{"$match":{"a":1}},{"$group":{"_id":"$b","n":{"$sum":1}}}],"cursor":{}}`,
	`{"aggregate":"c","pipeline":[{"$lookup":{"from":"d","pipeline":[{"$match":{}}],"as":"x"}},{"$facet":{"a":[{"$count":"n"}]}}]}`,
	`{"aggregate":1,"pipeline":[{"$documents":[{"a":1}]}]}`,
	`{"count":"c","query":{"a":{"$gt":1}}}`,
	`{"distinct":"c","key":"a"}`,
	`{"listCollections":1,"nameOnly":true}`,
	`{"listIndexes":"c"}`,
	`{"collStats":"c"}`,
	`{"dbStats":1}`,
	`{"explain":{"find":"c","filter":{}},"verbosity":"queryPlanner"}`,
	`{"explain":{"aggregate":"c","pipeline":[{"$match":{}}],"cursor":{}}}`,
	`{"explain":{"count":"c"}}`,
	`{"explain":{"distinct":"c","key":"a"}}`,
	`{"find":"c","readConcern":{"level":"majority"}}`,
	`{"find":"c","readConcern":{"level":"local"}}`,
	`{"find":"c","readConcern":{"level":"available"}}`,
	`{"find":"c","readConcern":{"level":"snapshot"}}`,
	`{"find":"c","readConcern":{}}`,
	`{"find":"c","comment":"$out is only text here"}`,
}

// readOnlyProbes are the write families and escape attempts the read-only
// guard must refuse with ErrReadOnly. They are shared with the integration
// suite, which also checks that nothing changed.
var readOnlyProbes = []struct{ name, cmd string }{
	{"insert", `{"insert":"c","documents":[{"_id":"probe"}]}`},
	{"update", `{"update":"c","updates":[{"q":{},"u":{"$set":{"x":1}},"multi":true}]}`},
	{"delete", `{"delete":"c","deletes":[{"q":{},"limit":0}]}`},
	{"findAndModify", `{"findAndModify":"c","query":{},"remove":true}`},
	{"findandmodify alias", `{"findandmodify":"c","query":{},"update":{"$set":{"x":1}}}`},
	{"bulkWrite", `{"bulkWrite":1,"ops":[{"delete":0,"filter":{}}],"nsInfo":[{"ns":"db.c"}]}`},
	{"aggregate $out", `{"aggregate":"c","pipeline":[{"$match":{}},{"$out":"copy"}],"cursor":{}}`},
	{"aggregate $merge", `{"aggregate":"c","pipeline":[{"$merge":{"into":"copy"}}],"cursor":{}}`},
	{"aggregate $out in $facet", `{"aggregate":"c","pipeline":[{"$facet":{"x":[{"$out":"copy"}]}}]}`},
	{"aggregate $merge in $lookup", `{"aggregate":"c","pipeline":[{"$lookup":{"from":"d","pipeline":[{"$merge":"copy"}],"as":"x"}}]}`},
	{"aggregate $out in $unionWith", `{"aggregate":"c","pipeline":[{"$unionWith":{"coll":"d","pipeline":[{"$out":"copy"}]}}]}`},
	{"aggregate db-level $out", `{"aggregate":1,"pipeline":[{"$documents":[{"a":1}]},{"$out":"copy"}]}`},
	{"aggregate duplicate pipeline", `{"aggregate":"c","pipeline":[{"$match":{}}],"pipeline":[{"$out":"copy"}]}`},
	{"aggregate stage with two keys", `{"aggregate":"c","pipeline":[{"$match":{},"$out":"copy"}]}`},
	{"explain delete", `{"explain":{"delete":"c","deletes":[{"q":{},"limit":0}]},"verbosity":"executionStats"}`},
	{"explain update", `{"explain":{"update":"c","updates":[{"q":{},"u":{"$set":{"x":1}}}]}}`},
	{"explain findAndModify", `{"explain":{"findAndModify":"c","query":{},"remove":true}}`},
	{"explain aggregate $out", `{"explain":{"aggregate":"c","pipeline":[{"$out":"copy"}],"cursor":{}}}`},
	{"explain of explain", `{"explain":{"explain":{"find":"c"}}}`},
	{"explain not a document", `{"explain":"find"}`},
	{"mapReduce", `{"mapReduce":"c","map":"function(){}","reduce":"function(){}","out":"copy"}`},
	{"mapReduce inline", `{"mapReduce":"c","map":"function(){}","reduce":"function(){}","out":{"inline":1}}`},
	{"eval", `{"eval":"db.c.drop()"}`},
	{"$eval", `{"$eval":"db.c.drop()"}`},
	{"create", `{"create":"newcoll"}`},
	{"createIndexes", `{"createIndexes":"c","indexes":[{"key":{"a":1},"name":"a_1"}]}`},
	{"drop", `{"drop":"c"}`},
	{"dropDatabase", `{"dropDatabase":1}`},
	{"dropIndexes", `{"dropIndexes":"c","index":"*"}`},
	{"renameCollection", `{"renameCollection":"db.c","to":"db.d"}`},
	{"collMod", `{"collMod":"c","validator":{}}`},
	{"convertToCapped", `{"convertToCapped":"c","size":1000}`},
	{"cloneCollectionAsCapped", `{"cloneCollectionAsCapped":"c","toCollection":"d","size":1000}`},
	{"applyOps", `{"applyOps":[{"op":"d","ns":"db.c","o":{"_id":1}}]}`},
	{"compact", `{"compact":"c"}`},
	{"createUser", `{"createUser":"x","pwd":"y","roles":[]}`},
	{"dropUser", `{"dropUser":"x"}`},
	{"grantRolesToUser", `{"grantRolesToUser":"x","roles":["root"]}`},
	{"setParameter", `{"setParameter":1,"notablescan":true}`},
	{"killOp", `{"killOp":1,"op":1}`},
	{"killCursors", `{"killCursors":"c","cursors":[]}`},
	{"shutdown", `{"shutdown":1}`},
	{"fsync", `{"fsync":1,"lock":true}`},
	{"getMore", `{"getMore":1,"collection":"c"}`},
	{"commitTransaction", `{"commitTransaction":1}`},
	{"case variant Find", `{"Find":"c"}`},
	{"case variant FINDANDMODIFY", `{"FINDANDMODIFY":"c","query":{},"remove":true}`},
	{"$db override", `{"find":"c","$db":"admin"}`},
	{"lsid", `{"find":"c","lsid":{"id":1}}`},
	{"txnNumber", `{"find":"c","txnNumber":1}`},
	{"startTransaction", `{"find":"c","startTransaction":true,"autocommit":false}`},
	{"writeConcern", `{"aggregate":"c","pipeline":[],"writeConcern":{"w":1}}`},
	{"duplicate command key", `{"find":"c","find":"d"}`},
	{"doTxn", `{"doTxn":[{"op":"d","ns":"db.c","o":{"_id":1}}],"txnNumber":1}`},
	{"endSessions", `{"endSessions":[{"id":{"$binary":{"base64":"AAAAAAAAAAAAAAAAAAAAAA==","subType":"04"}}}]}`},
	{"reIndex", `{"reIndex":"c"}`},
	{"validate", `{"validate":"c","full":true}`},
	{"find linearizable", `{"find":"c","readConcern":{"level":"linearizable"}}`},
	{"aggregate linearizable", `{"aggregate":"c","pipeline":[],"readConcern":{"level":"linearizable"}}`},
	{"explain find linearizable", `{"explain":{"find":"c","readConcern":{"level":"linearizable"}}}`},
	{"read concern unknown level", `{"count":"c","readConcern":{"level":"bogus"}}`},
	{"read concern not a document", `{"find":"c","readConcern":"local"}`},
}

func TestReadOnlyGuardAllows(t *testing.T) {
	for _, text := range readOnlyAllowed {
		cmd, err := parseCommand(text)
		if err != nil {
			t.Fatalf("parse %s: %v", text, err)
		}
		if err := checkReadOnly(cmd); err != nil {
			t.Errorf("checkReadOnly(%s): want nil, got %v", text, err)
		}
	}
}

func TestReadOnlyGuardRefuses(t *testing.T) {
	for _, p := range readOnlyProbes {
		t.Run(p.name, func(t *testing.T) {
			cmd, err := parseCommand(p.cmd)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			err = checkReadOnly(cmd)
			if !errors.Is(err, driver.ErrReadOnly) {
				t.Fatalf("checkReadOnly(%s): want ErrReadOnly, got %v", p.cmd, err)
			}
			var de *driver.Error
			if !errors.As(err, &de) || de.Driver != Name {
				t.Errorf("want a *driver.Error from %q, got %#v", Name, err)
			}
		})
	}
}
