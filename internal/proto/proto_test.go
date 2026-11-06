package proto_test

import (
	"testing"

	agentv1 "github.com/obleey/omegacd/internal/proto/agent/v1"
	sourcev1 "github.com/obleey/omegacd/internal/proto/source/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// Every message, with every field set, survives encode and decode; every Envelope body does too.
func TestRoundTrip(t *testing.T) {
	for _, fd := range []protoreflect.FileDescriptor{agentv1.File_omegacd_agent_v1_agent_proto, sourcev1.File_omegacd_source_v1_source_proto} {
		msgs := fd.Messages()
		for i := range msgs.Len() {
			m := newMessage(t, msgs.Get(i))
			fill(m)
			roundTrip(t, m.Interface())
		}
	}
	bodies := (&agentv1.Envelope{}).ProtoReflect().Descriptor().Oneofs().ByName("body").Fields()
	for i := range bodies.Len() {
		env := (&agentv1.Envelope{Id: 1}).ProtoReflect()
		body := env.NewField(bodies.Get(i))
		fill(body.Message())
		env.Set(bodies.Get(i), body)
		roundTrip(t, env.Interface())
	}
}

func newMessage(t *testing.T, md protoreflect.MessageDescriptor) protoreflect.Message {
	mt, err := protoregistry.GlobalTypes.FindMessageByName(md.FullName())
	if err != nil {
		t.Fatal(err)
	}
	return mt.New()
}

func roundTrip(t *testing.T, m proto.Message) {
	b, err := proto.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	got := m.ProtoReflect().New().Interface()
	if err := proto.Unmarshal(b, got); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(m, got) {
		t.Errorf("%s: got %v, want %v", m.ProtoReflect().Descriptor().FullName(), got, m)
	}
}

// fill sets every field to a non-zero value; of a oneof only the first member sticks.
func fill(m protoreflect.Message) {
	fields := m.Descriptor().Fields()
	for i := range fields.Len() {
		f := fields.Get(i)
		switch {
		case f.IsMap():
			m.Mutable(f).Map().Set(protoreflect.ValueOfString("k").MapKey(), scalar(f.MapValue()))
		case f.IsList():
			l := m.Mutable(f).List()
			if f.Message() != nil {
				v := l.NewElement()
				fill(v.Message())
				l.Append(v)
			} else {
				l.Append(scalar(f))
			}
		case f.Message() != nil:
			if f.ContainingOneof() == nil || m.WhichOneof(f.ContainingOneof()) == nil {
				fill(m.Mutable(f).Message())
			}
		case f.ContainingOneof() == nil || m.WhichOneof(f.ContainingOneof()) == nil:
			m.Set(f, scalar(f))
		}
	}
}

func scalar(f protoreflect.FieldDescriptor) protoreflect.Value {
	switch f.Kind() {
	case protoreflect.StringKind:
		return protoreflect.ValueOfString("x")
	case protoreflect.BytesKind:
		return protoreflect.ValueOfBytes([]byte{1, 2})
	case protoreflect.BoolKind:
		return protoreflect.ValueOfBool(true)
	case protoreflect.Uint32Kind:
		return protoreflect.ValueOfUint32(7)
	case protoreflect.Uint64Kind:
		return protoreflect.ValueOfUint64(7)
	case protoreflect.Int64Kind:
		return protoreflect.ValueOfInt64(-7)
	}
	panic("fill: add a value for " + f.Kind().String())
}
