package firestore

import (
	"context"
	"errors"
	"time"

	fs "cloud.google.com/go/firestore"

	"github.com/xavidop/tobira/driver"
)

var _ driver.Watcher = (*conn)(nil)

// Watch opens a snapshot listener on WatchRequest.Collection, or on the
// path WatchRequest.Channel names: a collection path (users, or
// users/ada/orders) or a document path (users/ada). Changes after the
// listener's first snapshot are Events: Op is insert, update or delete,
// Channel the collection path, Key {_id: id} and Payload the document's
// fields (its last fields for a delete). The channel closes when ctx ends;
// a listener error is sent as a last Event with Err. Each listener holds
// one streaming call open.
func (c *conn) Watch(ctx context.Context, req driver.WatchRequest) (<-chan driver.Event, error) {
	const op = "watch"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	path := req.Channel
	if req.Collection != nil {
		if err := c.checkNamespace(op, req.Collection.Namespace); err != nil {
			return nil, err
		}
		path = req.Collection.Name
	}
	if path == "" {
		return nil, syntaxErr(op, errors.New("name a collection, or a collection or document path as the channel"))
	}
	ch := make(chan driver.Event, 16)
	if checkPath(path, true) == nil {
		it := c.client.Doc(path).Snapshots(ctx)
		go c.watchDocument(ctx, it, ch)
		return ch, nil
	}
	if err := checkPath(path, false); err != nil {
		return nil, syntaxErr(op, err)
	}
	it := c.client.Collection(path).Snapshots(ctx)
	go c.watchQuery(ctx, path, it, ch)
	return ch, nil
}

// send delivers ev unless ctx ends first.
func send(ctx context.Context, ch chan<- driver.Event, ev driver.Event) bool {
	select {
	case ch <- ev:
		return true
	case <-ctx.Done():
		return false
	}
}

// endWatch sends a listener error as the last event, unless ctx ended.
func (c *conn) endWatch(ctx context.Context, ch chan<- driver.Event, err error) {
	if ctx.Err() == nil && !c.closed.Load() {
		send(ctx, ch, driver.Event{Time: time.Now().UTC(), Err: c.fail(ctx, "watch", err)})
	}
}

func (c *conn) watchQuery(ctx context.Context, path string, it *fs.QuerySnapshotIterator, ch chan<- driver.Event) {
	defer close(ch)
	defer it.Stop()
	first := true
	for {
		snap, err := it.Next()
		if err != nil {
			c.endWatch(ctx, ch, err)
			return
		}
		if first { // the current contents, not changes
			first = false
			continue
		}
		for _, chg := range snap.Changes {
			ev := driver.Event{Time: snap.ReadTime.UTC(), Channel: path, Key: driver.Object{{Key: idColumn, Value: driver.String(chg.Doc.Ref.ID)}}}
			switch chg.Kind {
			case fs.DocumentAdded:
				ev.Op = "insert"
			case fs.DocumentModified:
				ev.Op = "update"
			case fs.DocumentRemoved:
				ev.Op = "delete"
			}
			ev.Payload = payloadOf(chg.Doc)
			if !send(ctx, ch, ev) {
				return
			}
		}
	}
}

func (c *conn) watchDocument(ctx context.Context, it *fs.DocumentSnapshotIterator, ch chan<- driver.Event) {
	defer close(ch)
	defer it.Stop()
	first, existed := true, false
	var last *fs.DocumentSnapshot
	for {
		snap, err := it.Next()
		if err != nil {
			c.endWatch(ctx, ch, err)
			return
		}
		exists := snap.Exists()
		if first {
			first, existed, last = false, exists, snap
			continue
		}
		ev := driver.Event{
			Time:    snap.ReadTime.UTC(),
			Channel: relativePath(snap.Ref.Parent.Path),
			Key:     driver.Object{{Key: idColumn, Value: driver.String(snap.Ref.ID)}},
			Payload: driver.Null(),
		}
		switch {
		case exists && !existed:
			ev.Op, ev.Payload = "insert", payloadOf(snap)
		case exists:
			ev.Op, ev.Payload = "update", payloadOf(snap)
		case existed:
			ev.Op = "delete"
			if last != nil {
				ev.Payload = payloadOf(last)
			}
		default:
			continue
		}
		existed, last = exists, snap
		if !send(ctx, ch, ev) {
			return
		}
	}
}

// payloadOf is a document's fields as an Object.
func payloadOf(snap *fs.DocumentSnapshot) driver.Value {
	if snap == nil || !snap.Exists() {
		return driver.Null()
	}
	return driver.ObjectValue(rowFromSnapshot(snap).fields...)
}
