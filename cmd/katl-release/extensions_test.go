package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
)

const (
	drbd9Repository  = "ghcr.io/katl-dev/katl/extensions/drbd9"
	nvidiaRepository = "ghcr.io/katl-dev/katl/extensions/nvidia"
	finalDRBD9Tag    = "v2026.12.1-lts-x86_64-drbd9-9.3.4"
)

var (
	candidateDRBD9Digest = "sha256:" + strings.Repeat("a", 64)
	finalDRBD9Digest     = "sha256:" + strings.Repeat("b", 64)
)

// fakeRegistry is ghcr.io as an in-process transport: an OCI distribution
// registry that serves a public repository only to a bearer token from its
// anonymous token endpoint. It lists one tag per page, so a reader that
// ignores Link pagination misses tags. It needs no listener, so it also
// serves tests whose clock runs in a synctest bubble.
type fakeRegistry struct {
	t *testing.T

	mu sync.Mutex
	// tags maps a repository path to its tags and the manifest digest each
	// resolves to. A repository not listed does not exist.
	tags map[string]map[string]string
	// tokenStatus answers a repository's token request with an error status.
	tokenStatus map[string]int
	// tagStatus answers the next tag-list requests of a repository, one each.
	tagStatus map[string][]int
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	return &fakeRegistry{t: t, tags: map[string]map[string]string{}, tokenStatus: map[string]int{}, tagStatus: map[string][]int{}}
}

func (r *fakeRegistry) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" || req.URL.Host != "ghcr.io" {
		r.t.Errorf("request to %s, want https://ghcr.io", req.URL)
	}
	recorder := httptest.NewRecorder()
	r.serve(recorder, req)
	response := recorder.Result()
	response.Request = req
	return response, nil
}

// registryPath is the repository path of a full repository name.
func registryPath(repository string) string {
	_, name, _ := strings.Cut(repository, "/")
	return name
}

func registryError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"errors":[{"code":%q,"message":"fake"}]}`, code)
}

func (r *fakeRegistry) serve(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if req.URL.Path == "/token" {
		if req.Header.Get("Authorization") != "" {
			r.t.Errorf("token request carries credentials; the read must be anonymous")
		}
		name, ok := strings.CutPrefix(req.URL.Query().Get("scope"), "repository:")
		name, ok2 := strings.CutSuffix(name, ":pull")
		if !ok || !ok2 {
			registryError(w, http.StatusBadRequest, "UNSUPPORTED")
			return
		}
		if status := r.tokenStatus[name]; status != 0 {
			registryError(w, status, "DENIED")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"token":"anonymous %s"}`, name)
		return
	}

	rest, ok := strings.CutPrefix(req.URL.Path, "/v2/")
	if !ok {
		http.NotFound(w, req)
		return
	}
	name, reference, manifest := strings.Cut(rest, "/manifests/")
	if !manifest {
		name, ok = strings.CutSuffix(rest, "/tags/list")
		if !ok {
			http.NotFound(w, req)
			return
		}
	}
	if req.Header.Get("Authorization") != "Bearer anonymous "+name {
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="https://ghcr.io/token",service="ghcr.io",scope="repository:%s:pull"`, name))
		registryError(w, http.StatusUnauthorized, "UNAUTHORIZED")
		return
	}
	tags, exists := r.tags[name]

	if manifest {
		if req.Method != http.MethodHead || !strings.Contains(req.Header.Get("Accept"), "application/vnd.oci.image.manifest.v1+json") {
			r.t.Errorf("%s %s with Accept %q, want a HEAD that accepts OCI manifests", req.Method, req.URL.Path, req.Header.Get("Accept"))
		}
		digest, found := tags[reference]
		if !found {
			registryError(w, http.StatusNotFound, "MANIFEST_UNKNOWN")
			return
		}
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Docker-Content-Digest", digest)
		w.Header().Set("Content-Length", "512")
		return
	}

	if queue := r.tagStatus[name]; len(queue) > 0 {
		r.tagStatus[name] = queue[1:]
		code := "UNAVAILABLE"
		if queue[0] == http.StatusForbidden {
			code = "DENIED"
		}
		registryError(w, queue[0], code)
		return
	}
	if !exists {
		registryError(w, http.StatusNotFound, "NAME_UNKNOWN")
		return
	}
	names := make([]string, 0, len(tags))
	for tag := range tags {
		names = append(names, tag)
	}
	slices.Sort(names)
	last := req.URL.Query().Get("last")
	var page []string
	for _, tag := range names {
		if tag > last {
			page = []string{tag}
			break
		}
	}
	if len(page) == 1 && page[0] != names[len(names)-1] {
		w.Header().Set("Link", fmt.Sprintf(`</v2/%s/tags/list?last=%s>; rel="next"`, name, url.QueryEscape(page[0])))
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"name": name, "tags": page})
}

func recipe(repository string) []byte {
	return fmt.Appendf(nil, `{"version":"1","repository":%q,"sources":{}}`, repository)
}

// TestReaderExtensionReferences reads the published extension bundles while
// the GitHub Packages API refuses every request, as it does for the Release
// workflow's token and for tokens without read:packages.
func TestReaderExtensionReferences(t *testing.T) {
	final := []string{drbd9Repository + ":" + finalDRBD9Tag + "@" + finalDRBD9Digest}
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, f *fakeGitHub, r *reader)
		want  []string
		fails bool
	}{
		// Catches a digest taken from anything but the tag's manifest, a
		// reference outside the recipe's repository, a candidate tag counted
		// as the final's, and a tag list read without its later pages.
		{name: "published", want: final},
		// Catches discovery that does not use the recipes at the commit: a
		// second recipe whose repository the registry never created
		// publishes nothing, while drbd9 is still read.
		{name: "missing repository", want: final, setup: func(_ *testing.T, f *fakeGitHub, _ *reader) {
			addRecipe(f, "nvidia", nvidiaRepository)
			f.registry.tokenStatus[registryPath(nvidiaRepository)] = http.StatusNotFound
		}},
		// Catches a NAME_UNKNOWN from the tag list taken as a failure.
		{name: "repository without tags", setup: func(_ *testing.T, f *fakeGitHub, _ *reader) {
			delete(f.registry.tags, registryPath(drbd9Repository))
		}},
		// Catches a match on the version prefix alone, which takes the
		// candidate's tag for the final's.
		{name: "missing tag", setup: func(_ *testing.T, f *fakeGitHub, _ *reader) {
			delete(f.registry.tags[registryPath(drbd9Repository)], finalDRBD9Tag)
		}},
		// Catches a read without retries.
		{name: "transient failure", want: final, setup: func(_ *testing.T, f *fakeGitHub, _ *reader) {
			f.registry.tagStatus[registryPath(drbd9Repository)] = []int{http.StatusServiceUnavailable, http.StatusTooManyRequests}
		}},
		// Catches a server error read as "not published".
		{name: "persistent failure", fails: true, setup: func(_ *testing.T, f *fakeGitHub, _ *reader) {
			f.registry.tagStatus[registryPath(drbd9Repository)] = slices.Repeat([]int{http.StatusBadGateway}, readAttempts)
		}},
		// Catches a token refusal taken as a failure: ghcr.io denies an
		// anonymous token for a package that was never published, as it
		// does for a private one, while drbd9 is still read.
		{name: "never published", want: final, setup: func(_ *testing.T, f *fakeGitHub, _ *reader) {
			addRecipe(f, "nvidia", nvidiaRepository)
			f.registry.tokenStatus[registryPath(nvidiaRepository)] = http.StatusForbidden
		}},
		// Catches a refusal from the registry API, after a token was
		// issued, read as "not published".
		{name: "refused", fails: true, setup: func(_ *testing.T, f *fakeGitHub, _ *reader) {
			f.registry.tagStatus[registryPath(drbd9Repository)] = []int{http.StatusForbidden}
		}},
		// Catches a connection failure read as "not published".
		{name: "unreachable", fails: true, setup: func(t *testing.T, _ *fakeGitHub, r *reader) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			closed := listener.Addr().String()
			listener.Close()
			r.registry = &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, closed)
			}}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := readerWorld(t)
			r := newTestReader(f)
			if tc.setup != nil {
				tc.setup(t, f, r)
			}

			in, err := r.Read(context.Background(), "refs/heads/release/2026.12.1", readerHead, "")
			if tc.fails {
				if err == nil {
					t.Fatalf("read succeeded with extension references %q; want a read failure", in.ExtensionReferences)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(in.ExtensionReferences, tc.want) {
				t.Fatalf("extension references = %q, want %q", in.ExtensionReferences, tc.want)
			}
		})
	}
}

func addRecipe(f *fakeGitHub, name, repository string) {
	endpoint := repoPrefix + "contents/extensions?ref=" + readerHead
	f.responses[endpoint] = append(f.responses[endpoint].([]any), map[string]any{"name": name, "type": "dir"})
	f.raw[repoPrefix+"contents/extensions/"+name+"/recipe.json?ref="+readerHead] = recipe(repository)
}
