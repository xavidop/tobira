package convert_test

import (
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/xavidop/tobira/plugin/proto/tobira/plugin/v1"
)

func timestamp(sec int64, nanos int32) *timestamppb.Timestamp {
	return &timestamppb.Timestamp{Seconds: sec, Nanos: nanos}
}

func nullProto() *pb.Value { return &pb.Value{Kind: &pb.Value_Null{Null: &pb.Null{}}} }

func protoSize(m proto.Message) int { return proto.Size(m) }
