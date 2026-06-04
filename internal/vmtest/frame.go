package vmtest

import (
	"encoding/binary"
	"fmt"
	"io"

	"google.golang.org/protobuf/proto"
)

const defaultFrameLimit = 8 << 20

func writeProtoFrame(w io.Writer, message proto.Message) error {
	data, err := proto.Marshal(message)
	if err != nil {
		return err
	}
	if len(data) > defaultFrameLimit {
		return fmt.Errorf("vmtest frame too large: %d bytes", len(data))
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(data)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

func readProtoFrame(r io.Reader, message proto.Message) error {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size > defaultFrameLimit {
		return fmt.Errorf("vmtest frame exceeds limit: %d bytes", size)
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(r, data); err != nil {
		return err
	}
	return proto.Unmarshal(data, message)
}
