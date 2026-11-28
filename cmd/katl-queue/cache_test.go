package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/cachepolicy"
)

// testCachePolicy keeps two generations of go-, one of rpm-, and every blob-
// key. Budgets are in bytes so that fixture sizes can cross them.
const testCachePolicy = `{
  "version": 1,
  "save_ref": "refs/heads/main",
  "budget_bytes": 250,
  "alert_bytes": 240,
  "families": [
    {"name": "go", "key_prefix": "go-", "contents": "Go", "savers": [{"workflow": "ci.yml", "job": "checks"}], "budget_bytes": 100, "max_generations": 2},
    {"name": "rpm", "key_prefix": "rpm-", "contents": "RPMs", "savers": [{"workflow": "ci.yml", "job": "images"}], "budget_bytes": 100, "max_generations": 1},
    {"name": "blob", "key_prefix": "blob-", "contents": "Layers", "savers": [{"workflow": "ci.yml", "job": "builder"}], "budget_bytes": 50}
  ],
  "known_violations": []
}
`

var cacheNow = time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)

func cacheAt(id int64, ref, key string, size int64, created, accessed time.Duration) cacheEntry {
	return cacheEntry{ID: id, Ref: ref, Key: key, Size: size, Created: cacheNow.Add(-created), LastAccessed: cacheNow.Add(-accessed)}
}

const mainRef = "refs/heads/main"

// cacheFixture covers every class. Sizes put each family at or under its
// budget and the total under the alert threshold once the entries outside
// policy are gone.
func cacheFixture() []cacheEntry {
	return []cacheEntry{
		// go-a was created last but restored least recently, so it is the
		// oldest generation.
		cacheAt(1, mainRef, "go-a", 10, 1*time.Hour, 30*time.Hour),
		cacheAt(2, mainRef, "go-b", 10, 50*time.Hour, 2*time.Hour),
		cacheAt(3, mainRef, "go-c", 10, 40*time.Hour, 3*time.Hour),
		// Saved by a pull request and accessed most recently: it matches go
		// but is off-main, and must not take one of go's generations.
		cacheAt(4, "refs/pull/7/merge", "go-pr", 10, 1*time.Minute, 1*time.Minute),
		cacheAt(5, mainRef, "mystery-x", 10, time.Hour, time.Hour),
		cacheAt(6, "refs/heads/release/v1", "mystery-y", 10, time.Hour, time.Hour),
		// rpm-k1 has two entries, one stale. The key's newest access ranks
		// it, so both entries stay and rpm-k2 is the excess key.
		cacheAt(7, mainRef, "rpm-k1", 40, 60*time.Hour, 50*time.Hour),
		cacheAt(8, mainRef, "rpm-k1", 40, 5*time.Hour, 1*time.Hour),
		cacheAt(9, mainRef, "rpm-k2", 40, 2*time.Hour, 2*time.Hour),
		cacheAt(10, mainRef, "blob-1", 10, 90*time.Hour, 90*time.Hour),
		cacheAt(11, mainRef, "blob-2", 10, 1*time.Hour, 1*time.Hour),
		cacheAt(12, mainRef, "blob-3", 10, 9*time.Hour, 9*time.Hour),
	}
}

var cacheFixtureClasses = map[int64]string{
	1: cacheExcess, 2: cacheInPolicy, 3: cacheInPolicy, 4: cacheOffMain,
	5: cacheUndeclared, 6: cacheOffMain,
	7: cacheInPolicy, 8: cacheInPolicy, 9: cacheExcess,
	10: cacheInPolicy, 11: cacheInPolicy, 12: cacheInPolicy,
}

func outsidePolicy() []int64 {
	var ids []int64
	for id, class := range cacheFixtureClasses {
		if class != cacheInPolicy {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

func TestClassifyCaches(t *testing.T) {
	policy, err := cachepolicy.Parse([]byte(testCachePolicy))
	if err != nil {
		t.Fatal(err)
	}

	got := map[int64]string{}
	for _, entry := range classifyCaches(policy, cacheFixture()) {
		got[entry.ID] = entry.Class
	}
	if !maps.Equal(got, cacheFixtureClasses) {
		t.Errorf("classes = %v, want %v", got, cacheFixtureClasses)
	}
}

// TestClassifyCachesTies ranks keys accessed at the same time: the newer
// created_at keeps the generation, and key order settles a full tie. Each
// newer key sorts after the older one, so ranking by key alone keeps the
// wrong one.
func TestClassifyCachesTies(t *testing.T) {
	policy, err := cachepolicy.Parse([]byte(testCachePolicy))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		entries []cacheEntry
		excess  string
	}{
		{name: "newer created ranks first", entries: []cacheEntry{
			cacheAt(1, mainRef, "rpm-a", 1, 9*time.Hour, time.Hour),
			cacheAt(2, mainRef, "rpm-b", 1, 2*time.Hour, time.Hour),
		}, excess: "rpm-a"},
		// The key's newest creation across its entries ranks it, not the
		// creation of its most recently accessed entry.
		{name: "key's newest creation", entries: []cacheEntry{
			cacheAt(1, mainRef, "rpm-a", 1, 9*time.Hour, time.Hour),
			cacheAt(2, mainRef, "rpm-a", 1, 8*time.Hour, 5*time.Hour),
			cacheAt(3, mainRef, "rpm-b", 1, 2*time.Hour, time.Hour),
			cacheAt(4, mainRef, "rpm-b", 1, 3*time.Hour, 6*time.Hour),
		}, excess: "rpm-a"},
		{name: "key order", entries: []cacheEntry{
			cacheAt(1, mainRef, "rpm-b", 1, time.Hour, time.Hour),
			cacheAt(2, mainRef, "rpm-a", 1, time.Hour, time.Hour),
		}, excess: "rpm-b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, entry := range classifyCaches(policy, tc.entries) {
				if excess := entry.Key == tc.excess; excess != (entry.Class == cacheExcess) {
					t.Errorf("entry %d (%s) is %s, want excess only for %s", entry.ID, entry.Key, entry.Class, tc.excess)
				}
			}
		})
	}
}

type fakeCaches struct {
	entries   []cacheEntry
	remote    []byte
	remoteErr error
	listErr   error
	deleteErr map[int64]error
	// onDelete runs after each delete.
	onDelete func()

	listed  bool
	deleted []int64
}

func (f *fakeCaches) caches(context.Context) ([]cacheEntry, error) {
	f.listed = true
	return slices.Clone(f.entries), f.listErr
}

func (f *fakeCaches) defaultBranchPolicy(context.Context) ([]byte, error) {
	return f.remote, f.remoteErr
}

func (f *fakeCaches) deleteCache(_ context.Context, id int64) error {
	f.deleted = append(f.deleted, id)
	if f.onDelete != nil {
		f.onDelete()
	}
	return f.deleteErr[id]
}

// tableActions counts the first cells of the report's table rows, ignoring
// the padding tabwriter adds.
func tableActions(report string) map[string]int {
	actions := map[string]int{}
	for _, line := range strings.Split(report, "\n") {
		if cells := strings.Split(line, "|"); len(cells) > 2 {
			actions[strings.TrimSpace(cells[1])]++
		}
	}
	return actions
}

func TestCacheApply(t *testing.T) {
	src := &fakeCaches{entries: cacheFixture(), remote: []byte(testCachePolicy)}
	var out, errOut bytes.Buffer
	if err := cache(t.Context(), src, []byte(testCachePolicy), cacheOptions{apply: true, out: &out, errOut: &errOut}); err != nil {
		t.Fatalf("cache() = %v\n%s", err, out.String())
	}

	deleted := slices.Sorted(slices.Values(src.deleted))
	if !slices.Equal(deleted, outsidePolicy()) {
		t.Errorf("deleted %v, want exactly the entries outside policy %v", deleted, outsidePolicy())
	}

	actions := tableActions(out.String())
	if actions["deleted"] != len(outsidePolicy()) || actions["delete failed"] != 0 {
		t.Errorf("report actions %v, want %d deleted:\n%s", actions, len(outsidePolicy()), out.String())
	}
	for _, entry := range cacheFixture() {
		line := "deleted " + strconv.FormatInt(entry.ID, 10) + " " + entry.Key + "\n"
		if want := cacheFixtureClasses[entry.ID] != cacheInPolicy; strings.Contains(errOut.String(), line) != want {
			t.Errorf("progress has %q = %v, want %v:\n%s", line, !want, want, errOut.String())
		}
	}
}

func TestCacheReportDeletesNothing(t *testing.T) {
	// A differing remote policy proves that report mode does not consult it.
	src := &fakeCaches{entries: cacheFixture(), remote: []byte("{}")}
	var out, errOut bytes.Buffer
	if err := cache(t.Context(), src, []byte(testCachePolicy), cacheOptions{out: &out, errOut: &errOut}); err != nil {
		t.Fatalf("cache() = %v\n%s", err, out.String())
	}
	if len(src.deleted) > 0 || errOut.Len() > 0 {
		t.Errorf("report mode deleted %v:\n%s", src.deleted, errOut.String())
	}
	if got := tableActions(out.String())["would delete"]; got != len(outsidePolicy()) {
		t.Errorf("report lists %d entries to delete, want %d:\n%s", got, len(outsidePolicy()), out.String())
	}
}

func TestCacheApplyRefusal(t *testing.T) {
	for _, tc := range []struct {
		name      string
		remote    []byte
		remoteErr error
		want      string
	}{
		// The same policy with different bytes is still refused: the check
		// is byte identity, not equivalence.
		{name: "policy differs", remote: []byte(strings.TrimSuffix(testCachePolicy, "\n")), want: "differs from the default branch"},
		{name: "policy unreadable", remoteErr: errors.New("HTTP 502"), want: "refusing --apply"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := &fakeCaches{entries: cacheFixture(), remote: tc.remote, remoteErr: tc.remoteErr}
			var out, errOut bytes.Buffer
			err := cache(t.Context(), src, []byte(testCachePolicy), cacheOptions{apply: true, out: &out, errOut: &errOut})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("cache() = %v, want a refusal containing %q", err, tc.want)
			}
			if src.listed || len(src.deleted) > 0 {
				t.Errorf("refusal listed %v and deleted %v", src.listed, src.deleted)
			}
		})
	}
}

// TestCacheDeletePace cancels the run during the pause after the first
// delete: deleting without the pause would reach the rest first.
func TestCacheDeletePace(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	src := &fakeCaches{entries: cacheFixture(), remote: []byte(testCachePolicy), onDelete: cancel}
	var out, errOut bytes.Buffer

	err := cache(ctx, src, []byte(testCachePolicy), cacheOptions{apply: true, deletePace: time.Hour, out: &out, errOut: &errOut})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("cache() = %v, want cancellation", err)
	}
	if len(src.deleted) != 1 {
		t.Errorf("deleted %v, want one delete before the pause", src.deleted)
	}
}

func TestCacheJSONEmptyLists(t *testing.T) {
	src := &fakeCaches{remote: []byte(testCachePolicy)}
	var out, errOut bytes.Buffer
	if err := cache(t.Context(), src, []byte(testCachePolicy), cacheOptions{apply: true, asJSON: true, out: &out, errOut: &errOut}); err != nil {
		t.Fatalf("cache() = %v\n%s", err, out.String())
	}

	var report map[string]json.RawMessage
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"entries", "deleted", "problems"} {
		if got := string(report[field]); got != "[]" {
			t.Errorf("%s = %s, want []", field, got)
		}
	}
}

func TestCacheExit(t *testing.T) {
	grow := func(id, size int64) func([]cacheEntry) {
		return func(entries []cacheEntry) {
			for i := range entries {
				if entries[i].ID == id {
					entries[i].Size = size
				}
			}
		}
	}
	for _, tc := range []struct {
		name      string
		apply     bool
		change    func([]cacheEntry)
		listErr   error
		deleteErr map[int64]error
		code      int
		want      []string
	}{
		// The entries outside policy hold 80 of the fixture's 210 bytes,
		// which only what would remain is measured against.
		{name: "within policy once pruned", code: 0},
		{name: "family over budget", change: grow(11, 31), code: 1,
			want: []string{"family blob uses 51 B, over its 50 B budget"}},
		// Every family exactly at its budget, which is allowed, and the
		// total over the alert threshold.
		{name: "total over alert", change: func(e []cacheEntry) { grow(2, 80)(e); grow(3, 20)(e); grow(8, 60)(e); grow(10, 20)(e); grow(11, 20)(e) }, code: 1,
			want: []string{"caches use 250 B in total, over the 240 B alert threshold"}},
		{name: "budget not checked against entries to be deleted", change: grow(9, 1000), code: 0},
		{name: "list fails", listErr: errors.New("HTTP 502"), code: 2},
		// The undeleted entry still occupies rpm's budget.
		{name: "delete fails", apply: true, deleteErr: map[int64]error{9: errors.New("HTTP 403")}, code: 2,
			want: []string{"delete failed", "family rpm uses 120 B, over its 100 B budget"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entries := cacheFixture()
			if tc.change != nil {
				tc.change(entries)
			}
			src := &fakeCaches{entries: entries, remote: []byte(testCachePolicy), listErr: tc.listErr, deleteErr: tc.deleteErr}
			var out, errOut, stderr bytes.Buffer

			err := cache(t.Context(), src, []byte(testCachePolicy), cacheOptions{apply: tc.apply, out: &out, errOut: &errOut})
			if code := commandStatus(err, &stderr); code != tc.code {
				t.Errorf("exit %d, want %d (err %v)\n%s", code, tc.code, err, out.String())
			}
			for _, want := range tc.want {
				if !strings.Contains(out.String(), want) {
					t.Errorf("report lacks %q:\n%s", want, out.String())
				}
			}
		})
	}
}

func TestDecodeCaches(t *testing.T) {
	entry := func(id int, ref, key, accessed string) string {
		return fmt.Sprintf(`{"id":%d,"ref":%q,"key":%q,"size_in_bytes":1,"created_at":"2026-11-27T10:00:00Z","last_accessed_at":%q}`, id, ref, key, accessed)
	}
	const at = "2026-11-27T11:00:00Z"
	page := func(total int, entries ...string) string {
		return fmt.Sprintf(`{"total_count":%d,"actions_caches":[%s]}`, total, strings.Join(entries, ","))
	}
	for _, tc := range []struct {
		name  string
		pages []string
		ids   []int64
		err   string
	}{
		// An entry a shifted page repeats is counted once, so the listing
		// still matches its total.
		{name: "repeated entry", pages: []string{
			page(3, entry(1, mainRef, "go-a", at), entry(2, mainRef, "go-b", at)),
			page(3, entry(2, mainRef, "go-b", at), entry(3, mainRef, "go-c", at)),
		}, ids: []int64{1, 2, 3}},
		{name: "count differs from total", pages: []string{
			page(3, entry(1, mainRef, "go-a", at)),
			page(3, entry(1, mainRef, "go-a", at), entry(2, mainRef, "go-b", at)),
		}, err: "holds 2 entries but reports 3"},
		{name: "zero id", pages: []string{page(1, entry(0, mainRef, "go-a", at))}, err: "without an id"},
		{name: "empty ref", pages: []string{page(1, entry(1, "", "go-a", at))}, err: "lacks a ref or key"},
		{name: "empty key", pages: []string{page(1, entry(1, mainRef, "", at))}, err: "lacks a ref or key"},
		{name: "zero last access", pages: []string{page(1, entry(1, mainRef, "go-a", "0001-01-01T00:00:00Z"))}, err: "lacks last_accessed_at"},
		{name: "missing last access", pages: []string{page(1, `{"id":1,"ref":"refs/heads/main","key":"go-a"}`)}, err: "lacks last_accessed_at"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entries, err := decodeCaches([]byte("[" + strings.Join(tc.pages, ",") + "]"))
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("decodeCaches() = %v, %v, want error %q", entries, err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var ids []int64
			for _, entry := range entries {
				ids = append(ids, entry.ID)
			}
			if !slices.Equal(ids, tc.ids) {
				t.Errorf("ids = %v, want %v", ids, tc.ids)
			}
		})
	}
}

// fakeGHCases puts a gh on PATH that logs each invocation and answers through
// the given shell case arms, failing any other invocation. Unlike fakeGH, an
// arm can match any argument shape and keep per-call state. It returns a
// function that reads the log.
func fakeGHCases(t *testing.T, arms string) func() []string {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	script := `log=` + log + `
printf '%s\n' "$*" >>"$log"
case "$*" in
` + arms + `
  *) printf 'unexpected gh invocation: %s\n' "$*" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte("#!"+sh+"\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() []string {
		logged, err := os.ReadFile(log)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if len(logged) == 0 {
			return nil
		}
		return strings.Split(strings.TrimSuffix(string(logged), "\n"), "\n")
	}
}

const contentsRead = "api -H Accept: application/vnd.github.raw+json repos/katl-dev/katl/contents/.github/cache-policy.json --hostname github.com"

// TestCacheGitHub runs an apply through the gh client against a fake gh: a
// paginated listing, the raw contents read, a delete answered 404, a delete
// that succeeds after a server error, and one refused outright.
func TestCacheGitHub(t *testing.T) {
	policy := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(policy, []byte(testCachePolicy), 0o644); err != nil {
		t.Fatal(err)
	}
	logged := fakeGHCases(t, `
  "`+contentsRead+`")
    cat `+policy+` ;;
  "api --paginate --slurp repos/katl-dev/katl/actions/caches?per_page=100 --hostname github.com")
    printf '%s\n' '[
      {"total_count":4,"actions_caches":[
        {"id":1,"ref":"refs/heads/main","key":"go-a","size_in_bytes":10,"created_at":"2026-11-27T11:00:00Z","last_accessed_at":"2026-11-27T11:00:00Z"},
        {"id":2,"ref":"refs/pull/7/merge","key":"go-b","size_in_bytes":10,"created_at":"2026-11-27T11:00:00Z","last_accessed_at":"2026-11-27T11:00:00Z"}]},
      {"total_count":4,"actions_caches":[
        {"id":3,"ref":"refs/heads/main","key":"other","size_in_bytes":10,"created_at":"2026-11-27T11:00:00Z","last_accessed_at":"2026-11-27T11:00:00Z"},
        {"id":4,"ref":"refs/heads/x","key":"other","size_in_bytes":10,"created_at":"2026-11-27T11:00:00Z","last_accessed_at":"2026-11-27T11:00:00Z"}]}]' ;;
  "api -X DELETE repos/katl-dev/katl/actions/caches/2 --hostname github.com")
    echo 'gh: Not Found (HTTP 404)' >&2; exit 1 ;;
  "api -X DELETE repos/katl-dev/katl/actions/caches/3 --hostname github.com")
    if [ "$(grep -c 'caches/3 ' "$log")" -lt 2 ]; then echo 'gh: Bad Gateway (HTTP 502)' >&2; exit 1; fi ;;
  "api -X DELETE repos/katl-dev/katl/actions/caches/4 --hostname github.com")
    echo 'gh: Resource not accessible by integration (HTTP 403)' >&2; exit 1 ;;`)

	g := newGH()
	g.readDelay = time.Millisecond
	var out, errOut bytes.Buffer
	err := cache(t.Context(), g, []byte(testCachePolicy), cacheOptions{apply: true, asJSON: true, out: &out, errOut: &errOut})
	if err == nil || !strings.Contains(err.Error(), "delete cache 4") || strings.Contains(err.Error(), "cache 2") || strings.Contains(err.Error(), "cache 3") {
		t.Errorf("cache() = %v, want only the refused delete of 4 to fail", err)
	}

	deletes := map[string]int{}
	for _, line := range logged() {
		if id, ok := strings.CutPrefix(line, "api -X DELETE repos/katl-dev/katl/actions/caches/"); ok {
			deletes[strings.TrimSuffix(id, " --hostname github.com")]++
		}
	}
	// 1 is in policy; 2 counts as deleted on its first 404; 3 is retried
	// past the server error; 4 is refused and not retried.
	if want := map[string]int{"2": 1, "3": 2, "4": 1}; !maps.Equal(deletes, want) {
		t.Errorf("delete calls = %v, want %v", deletes, want)
	}

	var report cacheReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("decode report: %v\n%s", err, out.String())
	}
	if want := []int64{2, 3}; !slices.Equal(slices.Sorted(slices.Values(report.Deleted)), want) {
		t.Errorf("report deleted %v, want %v", report.Deleted, want)
	}
	classes := map[int64]string{}
	for _, entry := range report.Entries {
		classes[entry.ID] = entry.Class
	}
	if want := map[int64]string{1: cacheInPolicy, 2: cacheOffMain, 3: cacheUndeclared, 4: cacheOffMain}; !maps.Equal(classes, want) {
		t.Errorf("report classes %v, want %v", classes, want)
	}
	for _, want := range []string{"deleted 2 go-b\n", "deleted 3 other\n", "delete failed 4 other: "} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("progress lacks %q:\n%s", want, errOut.String())
		}
	}
}

// TestCacheGitHubPolicyMissing refuses an apply when the default branch holds
// no policy file, before listing or deleting anything.
func TestCacheGitHubPolicyMissing(t *testing.T) {
	logged := fakeGHCases(t, `
  "`+contentsRead+`")
    echo 'gh: Not Found (HTTP 404)' >&2; exit 1 ;;`)

	g := newGH()
	g.readDelay = time.Millisecond
	var out, errOut bytes.Buffer
	err := cache(t.Context(), g, []byte(testCachePolicy), cacheOptions{apply: true, out: &out, errOut: &errOut})
	if err == nil || !strings.Contains(err.Error(), "refusing --apply") {
		t.Errorf("cache() = %v, want a refusal", err)
	}
	if want := []string{contentsRead}; !slices.Equal(logged(), want) {
		t.Errorf("gh calls = %v, want only the contents read", logged())
	}
}
