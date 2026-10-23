package main

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Keep output failures visible even when a command ignores a formatting error.
type commandOutput struct {
	writer io.Writer
	err    error
}

func (w *commandOutput) Write(data []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	n, err := w.writer.Write(data)
	if err == nil && n < len(data) {
		err = io.ErrShortWrite
	}
	w.err = err
	return n, err
}

type tableWriter struct {
	writer *tabwriter.Writer
	err    error
}

func newTable(w io.Writer) *tableWriter {
	return &tableWriter{writer: tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)}
}

func (t *tableWriter) row(cells ...string) {
	if t.err == nil {
		_, t.err = fmt.Fprintln(t.writer, strings.Join(cells, "\t"))
	}
}

func (t *tableWriter) flush() error {
	if t.err != nil {
		return t.err
	}
	t.err = t.writer.Flush()
	return t.err
}

func writeProtoJSON(w io.Writer, name string, message proto.Message) error {
	data, err := (protojson.MarshalOptions{Multiline: true, Indent: "  "}).Marshal(message)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", name, err)
	}
	_, err = w.Write(append(data, '\n'))
	return err
}
