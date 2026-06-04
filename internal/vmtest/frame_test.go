package vmtest

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	vmtestpb "github.com/zariel/katl/internal/vmtest/proto"
)

func TestProtoFrameRoundTrip(t *testing.T) {
	var wire bytes.Buffer
	want := &vmtestpb.VmtestRequest{
		RequestId: "req-1",
		Operation: &vmtestpb.VmtestRequest_Health{
			Health: &vmtestpb.HealthRequest{},
		},
	}
	if err := writeProtoFrame(&wire, want); err != nil {
		t.Fatalf("writeProtoFrame() error = %v", err)
	}
	var got vmtestpb.VmtestRequest
	if err := readProtoFrame(&wire, &got); err != nil {
		t.Fatalf("readProtoFrame() error = %v", err)
	}
	if got.RequestId != want.RequestId {
		t.Fatalf("RequestId = %q", got.RequestId)
	}
	if _, ok := got.Operation.(*vmtestpb.VmtestRequest_Health); !ok {
		t.Fatalf("Operation = %T", got.Operation)
	}
}

func TestProtoFrameLimit(t *testing.T) {
	var wire bytes.Buffer
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], defaultFrameLimit+1)
	wire.Write(header[:])
	var got vmtestpb.VmtestRequest
	err := readProtoFrame(&wire, &got)
	if err == nil || !strings.Contains(err.Error(), "exceeds limit") {
		t.Fatalf("readProtoFrame() error = %v", err)
	}
}
