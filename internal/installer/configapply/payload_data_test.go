package configapply

import (
	"bytes"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestPayloadEncoding(t *testing.T) {
	encoded, err := yaml.Marshal(payloadData{0, 1, 254, 255})
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != "AAH+/w==\n" {
		t.Fatalf("encoded payload = %q, want one base64 scalar", encoded)
	}
}

func TestPayloadDecoding(t *testing.T) {
	var data payloadData
	if err := yaml.Unmarshal([]byte("AAH+/w==\n"), &data); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, []byte{0, 1, 254, 255}) {
		t.Fatalf("decoded payload = %v", data)
	}
}

func TestPayloadRejectsInvalidEncoding(t *testing.T) {
	for _, input := range []string{"[0, 1, 254, 255]", "not-base64!", "123"} {
		t.Run(input, func(t *testing.T) {
			var data payloadData
			if err := yaml.Unmarshal([]byte(input), &data); err == nil {
				t.Fatal("accepted invalid payload encoding")
			}
		})
	}
}
