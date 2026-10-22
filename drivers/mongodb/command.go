package mongodb

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/xavidop/tobira/driver"
)

// parseCommand reads a query's text: exactly one JSON object, in relaxed or
// canonical extended JSON, with at least one key (the command name). The
// order of keys is kept. Anything else is ErrSyntax.
func parseCommand(text string) (bson.D, error) {
	return parseCommandDoc(text, "query", false)
}

// parseCommandDoc reads one JSON object; allowEmpty accepts {}.
func parseCommandDoc(text, op string, allowEmpty bool) (bson.D, error) {
	bad := func(err error) (bson.D, error) {
		return nil, driver.Wrap(driver.ErrSyntax, Name, op, err)
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return bad(errors.New("empty document: give a JSON object such as {\"find\":\"orders\",\"filter\":{}}"))
	}
	if trimmed[0] != '{' {
		return bad(errors.New("expected one JSON object, such as {\"find\":\"orders\",\"filter\":{}}"))
	}
	dec := json.NewDecoder(strings.NewReader(trimmed))
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return bad(fmt.Errorf("invalid JSON: %w", err))
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return bad(errors.New("text after the JSON object: send one document"))
	}
	var cmd bson.D
	if err := bson.UnmarshalExtJSON(bytes.TrimSpace(raw), false, &cmd); err != nil {
		return bad(fmt.Errorf("invalid extended JSON: %w", err))
	}
	if len(cmd) == 0 && !allowEmpty {
		return bad(errors.New("empty command document: its first key names the command"))
	}
	if cmd == nil {
		cmd = bson.D{}
	}
	return cmd, nil
}

// reservedKeys are top-level fields the client manages. A command carrying
// one could run in another database or session than the driver chose.
var reservedKeys = map[string]bool{
	"lsid": true, "txnNumber": true, "startTransaction": true, "autocommit": true,
}

// cursorCommands return a cursor and run with RunCommandCursor.
var cursorCommands = map[string]bool{
	"find": true, "aggregate": true, "listCollections": true, "listIndexes": true,
}

// readConcernCommands are the reads that take a read concern.
var readConcernCommands = map[string]bool{
	"find": true, "aggregate": true, "count": true, "distinct": true,
}

// prepareCommand checks a parsed command and fills in what the driver adds:
//
//   - a read-only call must pass the guard (checkReadOnly) first, so any
//     refusal on a read-only call is ErrReadOnly;
//   - a top-level $-prefixed field ($db, $readPreference…) or a session
//     field (lsid, txnNumber…) is ErrSyntax on every call: the driver
//     chooses the database from Query.Namespace and manages sessions;
//   - a read-only find, aggregate, count or distinct gets read concern
//     "local" unless it names one;
//   - an aggregate without a cursor option gets "cursor": {} (unless it is
//     an explain), as the server requires.
//
// cmd is not modified; a copy is returned.
func prepareCommand(cmd bson.D, readOnly bool) (bson.D, error) {
	if readOnly {
		if err := checkReadOnly(cmd); err != nil {
			return nil, err
		}
	}
	for _, e := range cmd[1:] {
		if strings.HasPrefix(e.Key, "$") || reservedKeys[e.Key] {
			return nil, driver.Wrap(driver.ErrSyntax, Name, "query",
				fmt.Errorf("field %q is managed by tobira: choose the database with the namespace", e.Key))
		}
	}
	out := append(bson.D(nil), cmd...)
	name := out[0].Key
	if readOnly && readConcernCommands[name] {
		if _, ok := lookup(out, "readConcern"); !ok {
			out = append(out, bson.E{Key: "readConcern", Value: bson.D{{Key: "level", Value: "local"}}})
		}
	}
	if name == "aggregate" {
		_, hasCursor := lookup(out, "cursor")
		explain, _ := lookup(out, "explain")
		if !hasCursor && explain != true {
			out = append(out, bson.E{Key: "cursor", Value: bson.D{}})
		}
	}
	return out, nil
}

// lookup returns the value of the first field named key.
func lookup(d bson.D, key string) (any, bool) {
	for _, e := range d {
		if e.Key == key {
			return e.Value, true
		}
	}
	return nil, false
}

// commandTarget returns the collection a command names in its first field,
// or "" when it names none (a database-level command such as
// {"aggregate": 1}).
func commandTarget(cmd bson.D) string {
	if len(cmd) == 0 {
		return ""
	}
	s, _ := cmd[0].Value.(string)
	return s
}
