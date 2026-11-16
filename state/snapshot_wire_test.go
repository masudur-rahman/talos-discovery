package state

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// storageSchema rebuilds sidero.discovery.storage from its .proto definition,
// so the tests can check the hand-written encoding against a real protobuf
// implementation without generated code.
func storageSchema(t *testing.T) protoreflect.FileDescriptor {
	t.Helper()
	msg := func(name string, fields ...*descriptorpb.FieldDescriptorProto) *descriptorpb.DescriptorProto {
		return &descriptorpb.DescriptorProto{Name: proto.String(name), Field: fields}
	}
	field := func(name string, num int32, typ descriptorpb.FieldDescriptorProto_Type, typeName string, repeated bool) *descriptorpb.FieldDescriptorProto {
		label := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
		if repeated {
			label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED
		}
		f := &descriptorpb.FieldDescriptorProto{Name: proto.String(name), Number: proto.Int32(num), Type: typ.Enum(), Label: label.Enum(), JsonName: proto.String(name)}
		if typeName != "" {
			f.TypeName = proto.String(typeName)
		}
		return f
	}
	const (
		tMsg = descriptorpb.FieldDescriptorProto_TYPE_MESSAGE
		tStr = descriptorpb.FieldDescriptorProto_TYPE_STRING
		tByt = descriptorpb.FieldDescriptorProto_TYPE_BYTES
	)

	fd := &descriptorpb.FileDescriptorProto{
		Name:       proto.String("storage.proto"),
		Package:    proto.String("sidero.discovery.storage"),
		Syntax:     proto.String("proto3"),
		Dependency: []string{"google/protobuf/timestamp.proto", "google/protobuf/duration.proto"},
		MessageType: []*descriptorpb.DescriptorProto{
			msg("StateSnapshot", field("clusters", 1, tMsg, ".sidero.discovery.storage.ClusterSnapshot", true)),
			msg("ClusterSnapshot",
				field("id", 1, tStr, "", false),
				field("affiliates", 2, tMsg, ".sidero.discovery.storage.AffiliateSnapshot", true)),
			msg("AffiliateSnapshot",
				field("id", 1, tStr, "", false),
				field("expiration", 2, tMsg, ".google.protobuf.Timestamp", false),
				field("data", 3, tByt, "", false),
				field("endpoints", 4, tMsg, ".sidero.discovery.storage.EndpointSnapshot", true),
				field("ttl", 5, tMsg, ".google.protobuf.Duration", false)),
			msg("EndpointSnapshot",
				field("expiration", 1, tMsg, ".google.protobuf.Timestamp", false),
				field("data", 2, tByt, "", false),
				field("ttl", 3, tMsg, ".google.protobuf.Duration", false)),
		},
	}
	files, err := protodesc.NewFiles(&descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{
		protodesc.ToFileDescriptorProto(timestamppb.File_google_protobuf_timestamp_proto),
		protodesc.ToFileDescriptorProto(durationpb.File_google_protobuf_duration_proto),
		fd,
	}})
	if err != nil {
		t.Fatalf("build schema: %v", err)
	}
	file, err := files.FindFileByPath("storage.proto")
	if err != nil {
		t.Fatal(err)
	}
	return file
}

func TestSnapshotDecodesWithProtobuf(t *testing.T) {
	s, clock := newTestState()
	_ = s.Update("c1", "a1", []byte("data"), [][]byte{[]byte("ep")}, 90*time.Second+500*time.Millisecond)

	var buf bytes.Buffer
	if _, err := s.WriteSnapshot(&buf); err != nil {
		t.Fatal(err)
	}

	schema := storageSchema(t)
	snap := dynamicpb.NewMessage(schema.Messages().ByName("StateSnapshot"))
	if err := proto.Unmarshal(buf.Bytes(), snap); err != nil {
		t.Fatalf("protobuf rejects the snapshot: %v", err)
	}

	clusters := snap.Get(schema.Messages().ByName("StateSnapshot").Fields().ByName("clusters")).List()
	if clusters.Len() != 1 {
		t.Fatalf("clusters = %d", clusters.Len())
	}
	cluster := clusters.Get(0).Message()
	cd := cluster.Descriptor().Fields()
	if id := cluster.Get(cd.ByName("id")).String(); id != "c1" {
		t.Errorf("cluster id = %q", id)
	}
	aff := cluster.Get(cd.ByName("affiliates")).List().Get(0).Message()
	ad := aff.Descriptor().Fields()
	if d := aff.Get(ad.ByName("data")).Bytes(); string(d) != "data" {
		t.Errorf("data = %q", d)
	}

	ttl := aff.Get(ad.ByName("ttl")).Message()
	td := ttl.Descriptor().Fields()
	if secs, nanos := ttl.Get(td.ByName("seconds")).Int(), ttl.Get(td.ByName("nanos")).Int(); secs != 90 || nanos != 5e8 {
		t.Errorf("ttl = %ds %dns, want 90.5s", secs, nanos)
	}
	exp := aff.Get(ad.ByName("expiration")).Message()
	if secs := exp.Get(exp.Descriptor().Fields().ByName("seconds")).Int(); secs != clock.Now().Add(90*time.Second).Unix() {
		t.Errorf("expiration = %d, want now+90s", secs)
	}
}

// TestLegacySnapshotWithoutTTL covers files from writers that only set the
// absolute expiration: those must still load, and must age by the clock.
func TestLegacySnapshotWithoutTTL(t *testing.T) {
	s, clock := newTestState()
	schema := storageSchema(t)
	msgs := schema.Messages()

	ts := func(t time.Time) protoreflect.Value {
		m := dynamicpb.NewMessage(timestamppb.File_google_protobuf_timestamp_proto.Messages().ByName("Timestamp"))
		m.Set(m.Descriptor().Fields().ByName("seconds"), protoreflect.ValueOfInt64(t.Unix()))
		return protoreflect.ValueOfMessage(m)
	}

	aff := dynamicpb.NewMessage(msgs.ByName("AffiliateSnapshot"))
	ad := aff.Descriptor().Fields()
	aff.Set(ad.ByName("id"), protoreflect.ValueOfString("old"))
	aff.Set(ad.ByName("expiration"), ts(clock.Now().Add(time.Minute)))
	aff.Set(ad.ByName("data"), protoreflect.ValueOfBytes([]byte("d")))

	expired := dynamicpb.NewMessage(msgs.ByName("AffiliateSnapshot"))
	expired.Set(ad.ByName("id"), protoreflect.ValueOfString("gone"))
	expired.Set(ad.ByName("expiration"), ts(clock.Now().Add(-time.Minute)))

	cluster := dynamicpb.NewMessage(msgs.ByName("ClusterSnapshot"))
	cd := cluster.Descriptor().Fields()
	cluster.Set(cd.ByName("id"), protoreflect.ValueOfString("c1"))
	list := cluster.Mutable(cd.ByName("affiliates")).List()
	list.Append(protoreflect.ValueOfMessage(aff))
	list.Append(protoreflect.ValueOfMessage(expired))

	snap := dynamicpb.NewMessage(msgs.ByName("StateSnapshot"))
	snap.Mutable(snap.Descriptor().Fields().ByName("clusters")).List().Append(protoreflect.ValueOfMessage(cluster))
	raw, err := proto.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}

	st, err := s.ReadSnapshot(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("ReadSnapshot: %v", err)
	}
	if st.Affiliates != 1 {
		t.Errorf("affiliates = %d, want only the unexpired one", st.Affiliates)
	}
	if got := s.List("c1"); len(got) != 1 || got[0].ID != "old" {
		t.Errorf("List = %+v", got)
	}
}

func TestDecodeSnapshotJSON(t *testing.T) {
	s, _ := newTestState()
	_ = s.Update("c1", "a1", []byte("data"), [][]byte{[]byte("ep")}, time.Minute)
	var buf bytes.Buffer
	if _, err := s.WriteSnapshot(&buf); err != nil {
		t.Fatal(err)
	}

	out, err := DecodeSnapshotJSON(buf.Bytes())
	if err != nil {
		t.Fatalf("DecodeSnapshotJSON: %v", err)
	}
	var got struct {
		Clusters []struct {
			ID         string `json:"id"`
			Affiliates []struct {
				ID   string `json:"id"`
				TTL  string `json:"ttl"`
				Data []byte `json:"data"`
			} `json:"affiliates"`
		} `json:"clusters"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(got.Clusters) != 1 || got.Clusters[0].Affiliates[0].ID != "a1" || string(got.Clusters[0].Affiliates[0].Data) != "data" {
		t.Errorf("decoded = %s", out)
	}
	if ttl := got.Clusters[0].Affiliates[0].TTL; ttl == "" || ttl[len(ttl)-1] != 's' {
		t.Errorf("ttl = %q, want protojson duration form", ttl)
	}
}
