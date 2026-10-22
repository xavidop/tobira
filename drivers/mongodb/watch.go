package mongodb

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readconcern"

	"github.com/xavidop/tobira/driver"
)

var _ driver.Watcher = (*conn)(nil)

// changeStreamHint explains ErrUnsupported from Watch.
const changeStreamHint = "change streams need a replica set or a sharded cluster"

// Watch opens a change stream on WatchRequest.Collection, or on the
// database named by WatchRequest.Channel. Each change is an Event: Op is
// the operationType, Channel is "db.collection", Key the documentKey and
// Payload the full document (looked up for updates) or the update
// description. A change stream reads with read concern "majority", as
// MongoDB requires. On a standalone server it is ErrUnsupported. The channel
// closes when ctx ends; a stream error is sent as a last Event with Err.
func (c *conn) Watch(ctx context.Context, req driver.WatchRequest) (<-chan driver.Event, error) {
	const op = "watch"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	opts := options.ChangeStream().SetFullDocument(options.UpdateLookup)
	var cs *mongo.ChangeStream
	var err error
	switch {
	case req.Collection != nil:
		coll := c.database(req.Collection.Namespace).Collection(req.Collection.Name,
			options.Collection().SetReadConcern(readconcern.Majority()))
		cs, err = coll.Watch(ctx, mongo.Pipeline{}, opts)
	case req.Channel != "":
		db := c.client.Database(req.Channel, options.Database().SetReadConcern(readconcern.Majority()))
		cs, err = db.Watch(ctx, mongo.Pipeline{}, opts)
	default:
		return nil, driver.Wrap(driver.ErrSyntax, Name, op, errors.New("name a collection, or a database as the channel"))
	}
	if err != nil {
		err = mapError(ctx, op, err)
		var de *driver.Error
		if errors.Is(err, driver.ErrUnsupported) && errors.As(err, &de) {
			de.Err = fmt.Errorf("%s: %w", changeStreamHint, de.Err)
		}
		return nil, err
	}
	ch := make(chan driver.Event, 16)
	go func() {
		defer close(ch)
		defer func() {
			cctx, cancel := context.WithTimeout(context.Background(), cursorCloseTimeout)
			defer cancel()
			_ = cs.Close(cctx)
		}()
		for cs.Next(ctx) {
			select {
			case ch <- eventOf(cs.Current):
			case <-ctx.Done():
				return
			}
		}
		if err := cs.Err(); err != nil && ctx.Err() == nil {
			select {
			case ch <- driver.Event{Time: time.Now(), Err: mapError(ctx, op, err)}:
			case <-ctx.Done():
			}
		}
	}()
	return ch, nil
}

// eventOf maps a change event document to an Event.
func eventOf(doc bson.Raw) driver.Event {
	ev := driver.Event{Payload: driver.Null()}
	ev.Op, _ = doc.Lookup("operationType").StringValueOK()
	if t, ok := doc.Lookup("wallTime").TimeOK(); ok {
		ev.Time = t.UTC()
	} else if s, _, ok := doc.Lookup("clusterTime").TimestampOK(); ok {
		ev.Time = time.Unix(int64(s), 0).UTC()
	} else {
		ev.Time = time.Now().UTC()
	}
	if ns, ok := doc.Lookup("ns").DocumentOK(); ok {
		ev.Channel, _ = ns.Lookup("db").StringValueOK()
		if coll, ok := ns.Lookup("coll").StringValueOK(); ok {
			ev.Channel += "." + coll
		}
	}
	if k, ok := doc.Lookup("documentKey").DocumentOK(); ok {
		ev.Key = objectOf(k)
	}
	if v := doc.Lookup("fullDocument"); v.Type == bson.TypeEmbeddedDocument {
		ev.Payload = valueOf(v)
	} else if v := doc.Lookup("updateDescription"); v.Type == bson.TypeEmbeddedDocument {
		ev.Payload = valueOf(v)
	}
	return ev
}
