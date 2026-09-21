package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
)

func TestTableRows(t *testing.T) {
	var out bytes.Buffer
	table := newTable(&out)
	table.row("NODE", "STATE")
	table.row("a", "OK")
	table.row("long-node", "failed")
	table.row("", "detail")
	if err := table.flush(); err != nil {
		t.Fatal(err)
	}

	want := "NODE       STATE\na          OK\nlong-node  failed\n           detail\n"
	if out.String() != want {
		t.Fatalf("table = %q, want %q", out.String(), want)
	}
}

type failingOutput struct {
	err    error
	writes int
}

func (w *failingOutput) Write([]byte) (int, error) { w.writes++; return 0, w.err }

func TestTableWriteErrors(t *testing.T) {
	for _, duringRow := range []bool{false, true} {
		name := "flush"
		if duringRow {
			name = "row"
		}
		t.Run(name, func(t *testing.T) {
			failure := errors.New("output closed")
			out := &failingOutput{err: failure}
			table := newTable(out)
			value := "node"
			if duringRow {
				value += "\f"
			}
			table.row(value)
			if duringRow && out.writes == 0 {
				t.Fatal("fixture did not exercise a write before flush")
			}
			table.row("another node")
			if err := table.flush(); !errors.Is(err, failure) {
				t.Fatalf("flush = %v", err)
			}
			if err := table.flush(); !errors.Is(err, failure) {
				t.Fatalf("repeated flush lost output failure: %v", err)
			}
			if out.writes != 1 {
				t.Fatalf("attempted %d writes after an output failure", out.writes)
			}
		})
	}
}

func TestProtoJSON(t *testing.T) {
	var out bytes.Buffer
	if err := writeProtoJSON(&out, "generation", &agentapi.GenerationMutationResult{GenerationId: "g1"}); err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if want := map[string]any{"generationId": "g1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("JSON = %#v, want %#v", got, want)
	}
	if !strings.HasSuffix(out.String(), "\n") {
		t.Fatal("JSON lacks final newline")
	}
}

func TestProtoJSONErrors(t *testing.T) {
	t.Run("marshal", func(t *testing.T) {
		var out bytes.Buffer
		err := writeProtoJSON(&out, "generation", &agentapi.GenerationMutationResult{GenerationId: string([]byte{0xff})})
		if err == nil || !strings.Contains(err.Error(), "marshal generation") {
			t.Fatalf("error = %v", err)
		}
		if out.Len() != 0 {
			t.Fatalf("partial JSON = %q", out.String())
		}
	})
	t.Run("write", func(t *testing.T) {
		failure := errors.New("output closed")
		err := writeProtoJSON(&failingOutput{err: failure}, "generation", &agentapi.GenerationMutationResult{GenerationId: "g1"})
		if !errors.Is(err, failure) {
			t.Fatalf("error = %v", err)
		}
	})
}
