package cosmos

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/data/azcosmos"

	"github.com/xavidop/tobira/driver"
)

const (
	// pollInterval is how often Watch reads the change feed when it is idle.
	pollInterval = time.Second
	// feedPage is the most changes one change-feed read returns.
	feedPage = 100
)

// wholeContainer is the feed range covering every partition.
var wholeContainer = azcosmos.FeedRange{MinInclusive: "", MaxExclusive: "FF"}

// Watch follows a container's change feed (latest-version mode) from now:
// WatchRequest.Collection names the container, or Channel names one in the
// default database. Every created or replaced item arrives as an "upsert"
// event (the feed does not tell the two apart) with the item's key fields
// as Key and the item as Payload; deletes are not in this feed mode. The
// feed is polled every second. A read error is sent as the last event; the
// channel closes when ctx ends.
func (c *conn) Watch(ctx context.Context, req driver.WatchRequest) (<-chan driver.Event, error) {
	const op = "watch"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	var ref driver.CollectionRef
	switch {
	case req.Collection != nil:
		ref = *req.Collection
	case req.Channel != "":
		ref = driver.CollectionRef{Name: req.Channel}
	default:
		return nil, driver.Wrap(driver.ErrSyntax, Name, op, errors.New("name the container to watch"))
	}
	db, err := c.database(op, ref.Namespace)
	if err != nil {
		return nil, err
	}
	cc, err := c.container(op, db, ref.Name)
	if err != nil {
		return nil, err
	}
	props, err := c.containerProps(ctx, db, ref.Name, cc)
	if err != nil {
		return nil, mapError(ctx, op, err)
	}
	keys := keyPaths(props)
	now := time.Now()
	fr := wholeContainer
	first, err := cc.ReadChangeFeed(ctx, &azcosmos.ChangeFeedOptions{FeedRange: &fr, StartFrom: &now, MaxItemCount: feedPage})
	if err != nil {
		return nil, mapError(ctx, op, err)
	}
	ch := make(chan driver.Event, 16)
	go func() {
		defer close(ch)
		send := func(ev driver.Event) bool {
			select {
			case ch <- ev:
				return true
			case <-ctx.Done():
				return false
			}
		}
		resp := first
		token := first.ContinuationToken
		for {
			for _, raw := range resp.Items {
				ev, err := changeEvent(raw, keys, ref.Name)
				if err != nil {
					send(driver.Event{Time: time.Now(), Err: driver.Wrap(nil, Name, op, err)})
					return
				}
				if !send(ev) {
					return
				}
			}
			if len(resp.Items) < feedPage {
				select {
				case <-ctx.Done():
					return
				case <-time.After(pollInterval):
				}
			}
			opts := &azcosmos.ChangeFeedOptions{MaxItemCount: feedPage}
			if token != "" {
				tok := token
				opts.Continuation = &tok
			} else { // no token yet: keep reading from the start time
				opts.FeedRange, opts.StartFrom = &fr, &now
			}
			var err error
			resp, err = cc.ReadChangeFeed(ctx, opts)
			if err != nil {
				if ctx.Err() == nil {
					send(driver.Event{Time: time.Now(), Err: mapError(ctx, op, err)})
				}
				return
			}
			if resp.ContinuationToken != "" {
				token = resp.ContinuationToken
			}
		}
	}()
	return ch, nil
}

// changeEvent turns one change-feed item into an upsert event.
func changeEvent(raw []byte, keys []keyPath, container string) (driver.Event, error) {
	v, err := decodeValue(raw)
	if err != nil {
		return driver.Event{}, fmt.Errorf("cannot decode a change: %w", err)
	}
	doc, ok := v.AsObject()
	if !ok {
		return driver.Event{}, errors.New("a change is not an object")
	}
	ev := driver.Event{Time: time.Now(), Op: "upsert", Channel: container}
	if ts, ok := doc.Get("_ts"); ok {
		if n, ok := ts.AsInt(); ok {
			ev.Time = time.Unix(n, 0).UTC()
		}
	}
	for _, k := range keys {
		kv, _ := pkValueAt(doc, k.path)
		ev.Key = append(ev.Key, driver.Member{Key: k.name, Value: kv})
	}
	ev.Payload = driver.ObjectValue(doc...)
	return ev, nil
}
