package payloadbundle

import (
	"context"
	"io"
	"math"
	"strings"
	"testing"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestReadContentSize(t *testing.T) {
	for _, test := range []struct {
		name string
		size int64
	}{
		{
			name: "negative",
			size: -1,
		},
		{
			name: "oversized claim",
			size: math.MaxInt64,
		},
		{
			name: "trailing data",
			size: 3,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := ReadContent(context.Background(), contentSource("data"), ocispec.Descriptor{
				Digest: digest.FromString("data"),
				Size:   test.size,
			})
			if err == nil {
				t.Fatal("accepted content that does not match its declared size")
			}
		})
	}

	data, err := ReadContent(context.Background(), contentSource("data"), ocispec.Descriptor{
		Digest: digest.FromString("data"),
		Size:   4,
	})
	if err != nil || string(data) != "data" {
		t.Fatalf("valid content = %q, %v", data, err)
	}
}

type contentSource string

func (source contentSource) Fetch(context.Context, ocispec.Descriptor) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(string(source))), nil
}
