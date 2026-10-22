package mongodb

import (
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestEventOf(t *testing.T) {
	when := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)
	doc, _ := bson.Marshal(bson.D{
		{Key: "_id", Value: bson.D{{Key: "_data", Value: "x"}}},
		{Key: "operationType", Value: "insert"},
		{Key: "clusterTime", Value: bson.Timestamp{T: 1, I: 1}},
		{Key: "wallTime", Value: bson.NewDateTimeFromTime(when)},
		{Key: "ns", Value: bson.D{{Key: "db", Value: "shop"}, {Key: "coll", Value: "orders"}}},
		{Key: "documentKey", Value: bson.D{{Key: "_id", Value: int32(7)}}},
		{Key: "fullDocument", Value: bson.D{{Key: "_id", Value: int32(7)}, {Key: "n", Value: "x"}}},
	})
	ev := eventOf(doc)
	if ev.Op != "insert" || ev.Channel != "shop.orders" || !ev.Time.Equal(when) {
		t.Errorf("event %+v", ev)
	}
	if id, ok := ev.Key.Get("_id"); !ok || id.String() != "7" {
		t.Errorf("key %v", ev.Key)
	}
	if ev.Payload.String() != `{"_id":7,"n":"x"}` {
		t.Errorf("payload %v", ev.Payload)
	}

	upd, _ := bson.Marshal(bson.D{
		{Key: "operationType", Value: "update"},
		{Key: "clusterTime", Value: bson.Timestamp{T: 100, I: 1}},
		{Key: "ns", Value: bson.D{{Key: "db", Value: "shop"}}},
		{Key: "updateDescription", Value: bson.D{{Key: "updatedFields", Value: bson.D{{Key: "n", Value: 1}}}}},
	})
	ev = eventOf(upd)
	if ev.Op != "update" || ev.Channel != "shop" || ev.Time.Unix() != 100 || ev.Payload.String() != `{"updatedFields":{"n":1}}` {
		t.Errorf("update event %+v", ev)
	}
}
