package main

import (
	"maps"
	"testing"
)

func TestDecodeReferences(t *testing.T) {
	const data = `"data": {"repository": {"n1": {"__typename": "PullRequest", "state": "MERGED"}, "n2": null}}`

	t.Run("missing number", func(t *testing.T) {
		raw := `{` + data + `, "errors": [{"type": "NOT_FOUND", "message": "Could not resolve to an issue or pull request with the number of 2."}]}`

		refs, err := decodeReferences([]byte(raw), []int{1, 2})

		if err != nil {
			t.Fatal(err)
		}
		want := map[int]externalRef{1: {Pull: true, State: "MERGED"}}
		if !maps.Equal(refs, want) {
			t.Errorf("refs = %v, want %v, with the number that does not exist absent", refs, want)
		}
	})

	t.Run("other error", func(t *testing.T) {
		raw := `{` + data + `, "errors": [{"type": "NOT_FOUND", "message": "gone"}, {"type": "FORBIDDEN", "message": "Resource not accessible"}]}`

		refs, err := decodeReferences([]byte(raw), []int{1, 2})

		if err == nil {
			t.Errorf("refs = %v with no error; a failure other than NOT_FOUND must not read as a missing number", refs)
		}
	})

	t.Run("error without data", func(t *testing.T) {
		raw := `{"errors": [{"type": "RATE_LIMITED", "message": "API rate limit exceeded"}]}`

		if _, err := decodeReferences([]byte(raw), []int{1}); err == nil {
			t.Error("a rate limit read as no pull requests")
		}
	})
}
