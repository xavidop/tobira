package dynamodb

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/dynamodbstreams"
	streamtypes "github.com/aws/aws-sdk-go-v2/service/dynamodbstreams/types"

	"github.com/xavidop/tobira/driver"
)

const (
	// watchPoll is the pause between two rounds of GetRecords.
	watchPoll = time.Second
	// rediscoverEvery is how many rounds pass between two DescribeStream
	// calls when no shard closed.
	rediscoverEvery = 10
)

var _ driver.Watcher = (*conn)(nil)

// Watch streams a table's changes from DynamoDB Streams: the table is
// WatchRequest.Collection, or WatchRequest.Channel names it. The table
// must have a stream (StreamSpecification enabled), else ErrUnsupported.
// Watch positions every open shard at its tip (LATEST) before it returns,
// so every change after that is seen; shards that open later are read from
// their start. It polls each shard once a second. Each event's Op is
// insert, update or delete, Channel the table, Key the item's key, Payload
// the new image (the old image for a delete; NULL when the stream's view
// type holds neither) and Time the approximate change time. Shards are
// read in order of their starting sequence number, and a shard only once
// its parent has no more records, so a key's changes arrive in order
// across shard rollovers. The channel closes when ctx ends or the
// connection closes; a stream error is sent as a last event.
func (c *conn) Watch(ctx context.Context, req driver.WatchRequest) (<-chan driver.Event, error) {
	const op = "watch"
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	table := req.Channel
	if req.Collection != nil {
		if err := checkNamespace(req.Collection.Namespace, op); err != nil {
			return nil, err
		}
		table = req.Collection.Name
	}
	if table == "" {
		return nil, driver.Wrap(driver.ErrSyntax, Name, op, errors.New("name the table to watch"))
	}
	out, err := c.db.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(table)})
	if err != nil {
		return nil, c.fail(ctx, op, err)
	}
	spec := out.Table.StreamSpecification
	if out.Table.LatestStreamArn == nil || spec == nil || !aws.ToBool(spec.StreamEnabled) {
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, fmt.Errorf(
			"table %q has no stream: enable DynamoDB Streams on it (view type NEW_AND_OLD_IMAGES)", table))
	}
	w := &watcher{c: c, table: table, arn: out.Table.LatestStreamArn, shards: map[string]*shardState{}, seen: map[string]bool{}}
	if err := w.discover(ctx, streamtypes.ShardIteratorTypeLatest); err != nil {
		return nil, err
	}
	ch := make(chan driver.Event)
	go w.run(ctx, ch)
	return ch, nil
}

// watcher polls the shards of one stream.
type watcher struct {
	c      *conn
	table  string
	arn    *string
	shards map[string]*shardState // shard id -> state, while it has records to read
	seen   map[string]bool        // every shard id met so far
}

// shardState is a shard being read: its next iterator, its starting
// sequence number and its parent shard id ("" when none).
type shardState struct {
	iter   *string
	start  string
	parent string
}

// readyShards returns the shards to read this round, ordered by starting
// sequence number (decimal strings, compared numerically): every shard
// whose parent is no longer being read. A child waits until its parent's
// last records are out.
func readyShards(shards map[string]*shardState) []string {
	var ids []string
	for id, s := range shards {
		if _, parentOpen := shards[s.parent]; s.parent != "" && parentOpen {
			continue
		}
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b string) int {
		if c := compareSequence(shards[a].start, shards[b].start); c != 0 {
			return c
		}
		return strings.Compare(a, b)
	})
	return ids
}

// compareSequence orders two decimal sequence numbers.
func compareSequence(a, b string) int {
	if len(a) != len(b) {
		return len(a) - len(b)
	}
	return strings.Compare(a, b)
}

// discover lists the stream's shards and takes an iterator for each one
// not met before. On the first call (LATEST) shards already closed are
// only noted; later new shards are read from their start (TRIM_HORIZON).
func (w *watcher) discover(ctx context.Context, from streamtypes.ShardIteratorType) error {
	const op = "watch"
	in := &dynamodbstreams.DescribeStreamInput{StreamArn: w.arn}
	for {
		out, err := w.c.streams.DescribeStream(ctx, in)
		if err != nil {
			return w.c.fail(ctx, op, err)
		}
		var last *string
		for _, sh := range out.StreamDescription.Shards {
			id := aws.ToString(sh.ShardId)
			last = sh.ShardId
			if w.seen[id] {
				continue
			}
			w.seen[id] = true
			closed := sh.SequenceNumberRange != nil && sh.SequenceNumberRange.EndingSequenceNumber != nil
			if from == streamtypes.ShardIteratorTypeLatest && closed {
				continue
			}
			it, err := w.c.streams.GetShardIterator(ctx, &dynamodbstreams.GetShardIteratorInput{
				StreamArn: w.arn, ShardId: sh.ShardId, ShardIteratorType: from,
			})
			if err != nil {
				return w.c.fail(ctx, op, err)
			}
			st := &shardState{iter: it.ShardIterator, parent: aws.ToString(sh.ParentShardId)}
			if sh.SequenceNumberRange != nil {
				st.start = aws.ToString(sh.SequenceNumberRange.StartingSequenceNumber)
			}
			w.shards[id] = st
		}
		if out.StreamDescription.LastEvaluatedShardId == nil || last == nil {
			return nil
		}
		in.ExclusiveStartShardId = out.StreamDescription.LastEvaluatedShardId
	}
}

func (w *watcher) run(ctx context.Context, ch chan<- driver.Event) {
	defer close(ch)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { // end with the connection
		select {
		case <-w.c.done:
			cancel()
		case <-ctx.Done():
		}
	}()
	timer := time.NewTimer(0)
	defer timer.Stop()
	for round := 1; ; round++ {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		rediscover := round%rediscoverEvery == 0
		for _, id := range readyShards(w.shards) {
			st := w.shards[id]
			out, err := w.c.streams.GetRecords(ctx, &dynamodbstreams.GetRecordsInput{ShardIterator: st.iter})
			if err != nil {
				w.stop(ctx, ch, err)
				return
			}
			for _, rec := range out.Records {
				if !w.send(ctx, ch, eventOf(w.table, rec)) {
					return
				}
			}
			if out.NextShardIterator == nil {
				delete(w.shards, id)
				rediscover = true
				continue
			}
			st.iter = out.NextShardIterator
		}
		if rediscover {
			if err := w.discover(ctx, streamtypes.ShardIteratorTypeTrimHorizon); err != nil {
				w.stop(ctx, ch, err)
				return
			}
		}
		timer.Reset(watchPoll)
	}
}

// stop sends a terminal error, unless ctx ended (then the error is ours).
func (w *watcher) stop(ctx context.Context, ch chan<- driver.Event, err error) {
	if ctx.Err() != nil {
		return
	}
	w.send(ctx, ch, driver.Event{Time: time.Now(), Channel: w.table, Err: w.c.fail(ctx, "watch", err)})
}

func (w *watcher) send(ctx context.Context, ch chan<- driver.Event, ev driver.Event) bool {
	select {
	case ch <- ev:
		return true
	case <-ctx.Done():
		return false
	}
}

var streamOps = map[streamtypes.OperationType]string{
	streamtypes.OperationTypeInsert: "insert",
	streamtypes.OperationTypeModify: "update",
	streamtypes.OperationTypeRemove: "delete",
}

func eventOf(table string, rec streamtypes.Record) driver.Event {
	ev := driver.Event{Op: streamOps[rec.EventName], Channel: table, Payload: driver.Null()}
	if ev.Op == "" {
		ev.Op = string(rec.EventName)
	}
	sr := rec.Dynamodb
	if sr == nil {
		return ev
	}
	if sr.ApproximateCreationDateTime != nil {
		ev.Time = *sr.ApproximateCreationDateTime
	}
	if len(sr.Keys) > 0 {
		ev.Key, _ = valueOf(&types.AttributeValueMemberM{Value: fromStreamItem(sr.Keys)}).AsObject()
	}
	image := sr.NewImage
	if rec.EventName == streamtypes.OperationTypeRemove {
		image = sr.OldImage
	}
	if image != nil {
		ev.Payload = valueOf(&types.AttributeValueMemberM{Value: fromStreamItem(image)})
	}
	return ev
}

func fromStreamItem(m map[string]streamtypes.AttributeValue) item {
	out := make(item, len(m))
	for k, v := range m {
		out[k] = fromStream(v)
	}
	return out
}

// fromStream converts a Streams attribute value to the DynamoDB one.
func fromStream(av streamtypes.AttributeValue) types.AttributeValue {
	switch v := av.(type) {
	case *streamtypes.AttributeValueMemberS:
		return &types.AttributeValueMemberS{Value: v.Value}
	case *streamtypes.AttributeValueMemberN:
		return &types.AttributeValueMemberN{Value: v.Value}
	case *streamtypes.AttributeValueMemberB:
		return &types.AttributeValueMemberB{Value: v.Value}
	case *streamtypes.AttributeValueMemberBOOL:
		return &types.AttributeValueMemberBOOL{Value: v.Value}
	case *streamtypes.AttributeValueMemberNULL:
		return &types.AttributeValueMemberNULL{Value: v.Value}
	case *streamtypes.AttributeValueMemberSS:
		return &types.AttributeValueMemberSS{Value: v.Value}
	case *streamtypes.AttributeValueMemberNS:
		return &types.AttributeValueMemberNS{Value: v.Value}
	case *streamtypes.AttributeValueMemberBS:
		return &types.AttributeValueMemberBS{Value: v.Value}
	case *streamtypes.AttributeValueMemberL:
		out := make([]types.AttributeValue, len(v.Value))
		for i, e := range v.Value {
			out[i] = fromStream(e)
		}
		return &types.AttributeValueMemberL{Value: out}
	case *streamtypes.AttributeValueMemberM:
		return &types.AttributeValueMemberM{Value: fromStreamItem(v.Value)}
	}
	return &types.AttributeValueMemberNULL{Value: true}
}
