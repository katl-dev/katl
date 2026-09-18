package kubernetesrelease

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestDiscover(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "1" {
			w.Header().Set("Link", `<https://example.invalid/releases?page=2>; rel="next"`)
			fmt.Fprint(w, `[{"tag_name":"v1.40.0-rc.1"},{"tag_name":"v1.39.0","prerelease":true},{"tag_name":"v1.38.0","draft":true},{"tag_name":"v1.37.0"},{"tag_name":"v1.36.9"}]`)
		} else {
			fmt.Fprint(w, `[{"tag_name":"v1.36.10"},{"tag_name":"v1.35.12"},{"tag_name":"v1.34.20"},{"tag_name":"v1.37.0"}]`)
		}
	}))
	defer server.Close()

	got, err := Discover(context.Background(), server.Client(), server.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"v1.37.0", "v1.36.10", "v1.35.12"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("releases = %v, want %v", got, want)
	}
}

func TestDiscoverFailure(t *testing.T) {
	for _, response := range []struct {
		name, body string
		status     int
	}{
		{"unavailable", `[]`, 503}, {"empty", `[]`, 200}, {"malformed", `{}`, 200},
	} {
		t.Run(response.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(response.status)
				fmt.Fprint(w, response.body)
			}))
			defer server.Close()
			if _, err := Discover(context.Background(), server.Client(), server.URL, ""); err == nil {
				t.Fatal("invalid upstream response accepted")
			}
		})
	}
}
