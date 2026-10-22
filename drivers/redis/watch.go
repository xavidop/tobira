package redis

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/xavidop/tobira/driver"
)

// notifyClasses are the notify-keyspace-events letters that select events.
const notifyClasses = "g$lshzxetmdnA"

// notifyModes reads a notify-keyspace-events value: whether keyspace (K)
// and keyevent (E) notifications are published, each needing at least one
// event class.
func notifyModes(flags string) (keyspace, keyevent bool) {
	class := strings.ContainsAny(flags, notifyClasses)
	return class && strings.Contains(flags, "K"), class && strings.Contains(flags, "E")
}

// Watch streams keyspace notifications for a collection's keys
// (req.Collection) or for keys matching a glob (req.Channel). It never
// changes server configuration: when notify-keyspace-events is off, it
// returns ErrUnsupported. When CONFIG GET is refused (managed services), it
// subscribes anyway. Op is "insert" for a new key, "delete" for del,
// expired and evicted, and "update" otherwise; Payload is the native event
// name. The channel closes when ctx ends.
func (c *conn) Watch(ctx context.Context, req driver.WatchRequest) (<-chan driver.Event, error) {
	if err := c.check(ctx, "watch"); err != nil {
		return nil, err
	}
	ns, coll, glob := "", "", ""
	switch {
	case req.Collection != nil:
		ns, coll = req.Collection.Namespace, req.Collection.Name
	case req.Channel != "":
		glob = req.Channel
	default:
		return nil, driver.Wrap(driver.ErrSyntax, Name, "watch", errors.New("watch needs a collection or a key pattern"))
	}
	cl, db, release, err := c.client("watch", ns)
	if err != nil {
		return nil, err
	}
	started := false
	defer func() {
		if !started {
			release()
		}
	}()
	keyspace := true
	if cfg, err := cl.ConfigGet(ctx, "notify-keyspace-events").Result(); err == nil {
		ks, ev := notifyModes(cfg["notify-keyspace-events"])
		if !ks && !ev {
			return nil, driver.Wrap(driver.ErrUnsupported, Name, "watch", errors.New(
				"keyspace notifications are off (notify-keyspace-events is empty): ask an administrator to enable them, "+
					"for example with CONFIG SET notify-keyspace-events KEA; tobira never changes server configuration"))
		}
		keyspace = ks
	} else if ctx.Err() != nil {
		return nil, mapError(ctx, "watch", ctx.Err())
	}
	match := func(key string) bool {
		if glob != "" {
			ok, _ := globMatch(ctx, glob, key) // a cancelled ctx ends the loop anyway
			return ok
		}
		return inCollection(coll, key)
	}
	prefix := "__keyevent@" + itoa(db) + "__:"
	pattern := prefix + "*"
	if keyspace {
		prefix = "__keyspace@" + itoa(db) + "__:"
		if glob != "" {
			pattern = prefix + glob
		} else {
			pattern = prefix + matchPattern(coll)
		}
	}
	ps := cl.PSubscribe(ctx, pattern)
	if _, err := ps.Receive(ctx); err != nil { // the subscription confirmation, or why it failed
		_ = ps.Close()
		return nil, mapError(ctx, "watch", err)
	}
	msgs := ps.Channel()
	out := make(chan driver.Event, watchBuffer)
	started = true
	go func() {
		defer release()
		defer close(out)
		defer func() { _ = ps.Close() }()
		for {
			select {
			case <-ctx.Done():
				return
			case m, ok := <-msgs:
				if !ok {
					select {
					case out <- driver.Event{Time: time.Now(), Err: driver.Wrap(driver.ErrConnection, Name, "watch", errors.New("subscription closed"))}:
					case <-ctx.Done():
					}
					return
				}
				rest := strings.TrimPrefix(m.Channel, prefix)
				key, event := rest, m.Payload
				if !keyspace {
					key, event = m.Payload, rest
				}
				if !match(key) {
					continue
				}
				ev := driver.Event{
					Time: time.Now(), Op: eventOp(event), Channel: key,
					Key: driver.Object{{Key: colKey, Value: textValue(key)}}, Payload: driver.String(event),
				}
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

// eventOp maps a keyspace event name to an Event.Op.
func eventOp(event string) string {
	switch event {
	case "new":
		return "insert"
	case "del", "expired", "evicted":
		return "delete"
	}
	return "update"
}
