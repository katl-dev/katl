package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/katl-dev/katl/internal/cachepolicy"
)

const cachePolicyPath = ".github/cache-policy.json"

// cacheEntry is one GitHub Actions cache entry as the caches API lists it.
type cacheEntry struct {
	ID           int64     `json:"id"`
	Ref          string    `json:"ref"`
	Key          string    `json:"key"`
	Size         int64     `json:"size_in_bytes"`
	Created      time.Time `json:"created_at"`
	LastAccessed time.Time `json:"last_accessed_at"`
}

const (
	cacheInPolicy   = "in-policy"
	cacheOffMain    = "off-main"
	cacheUndeclared = "undeclared"
	cacheExcess     = "excess-generation"
)

type classifiedCache struct {
	cacheEntry
	Family string `json:"family,omitempty"`
	Class  string `json:"class"`
}

// cacheSource is the GitHub state the cache command reads and the one change
// it makes.
type cacheSource interface {
	caches(ctx context.Context) ([]cacheEntry, error)
	// defaultBranchPolicy returns the policy file on the default branch.
	defaultBranchPolicy(ctx context.Context) ([]byte, error)
	// deleteCache succeeds when the entry is gone, including when it was
	// already gone.
	deleteCache(ctx context.Context, id int64) error
}

// classifyCaches gives each entry one class, tested in order: saved off the
// policy's save ref, matching no family, then beyond its family's newest
// max_generations keys. A key's age is its most recently accessed entry, so a
// key restored yesterday outranks one created later but never used; between
// keys accessed at the same time, the more recently created one ranks first.
func classifyCaches(policy *cachepolicy.Policy, entries []cacheEntry) []classifiedCache {
	out := make([]classifiedCache, len(entries))
	// newest holds, per family and key, the key's latest access and
	// creation across its entries.
	type keyAge struct{ accessed, created time.Time }
	newest := map[string]map[string]keyAge{}
	for i, entry := range entries {
		out[i].cacheEntry = entry
		family, ok := policy.Family(entry.Key)
		if ok {
			out[i].Family = family.Name
		}
		switch {
		case entry.Ref != policy.SaveRef:
			out[i].Class = cacheOffMain
		case !ok:
			out[i].Class = cacheUndeclared
		default:
			out[i].Class = cacheInPolicy
			if newest[family.Name] == nil {
				newest[family.Name] = map[string]keyAge{}
			}
			age := newest[family.Name][entry.Key]
			if entry.LastAccessed.After(age.accessed) {
				age.accessed = entry.LastAccessed
			}
			if entry.Created.After(age.created) {
				age.created = entry.Created
			}
			newest[family.Name][entry.Key] = age
		}
	}

	excess := map[string]bool{}
	for _, family := range policy.Families {
		keys := newest[family.Name]
		if family.MaxGenerations == nil || len(keys) <= *family.MaxGenerations {
			continue
		}
		ranked := slices.Collect(maps.Keys(keys))
		slices.SortFunc(ranked, func(a, b string) int {
			return cmp.Or(keys[b].accessed.Compare(keys[a].accessed), keys[b].created.Compare(keys[a].created), strings.Compare(a, b))
		})
		for _, key := range ranked[*family.MaxGenerations:] {
			excess[key] = true
		}
	}
	for i := range out {
		if out[i].Class == cacheInPolicy && excess[out[i].Key] {
			out[i].Class = cacheExcess
		}
	}
	return out
}

type cacheFamilyUsage struct {
	Name        string `json:"name"`
	Keys        int    `json:"keys"`
	Entries     int    `json:"entries"`
	Bytes       int64  `json:"bytes"`
	BudgetBytes int64  `json:"budget_bytes"`
}

type cacheReport struct {
	Applied bool              `json:"applied"`
	Entries []classifiedCache `json:"entries"`
	// Deleted lists the ids this run deleted.
	Deleted     []int64            `json:"deleted"`
	Families    []cacheFamilyUsage `json:"families"`
	TotalBytes  int64              `json:"total_bytes"`
	BudgetBytes int64              `json:"budget_bytes"`
	AlertBytes  int64              `json:"alert_bytes"`
	Problems    []string           `json:"problems"`
}

func cacheCommand(ctx context.Context, flags *flag.FlagSet, args []string, out, errOut io.Writer) error {
	apply := flags.Bool("apply", false, "delete the entries outside policy instead of reporting them")
	asJSON := flags.Bool("json", false, "print the report as JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("cache takes no positional arguments")
	}
	local, err := os.ReadFile(cachePolicyPath)
	if err != nil {
		return fmt.Errorf("read the cache policy from the checkout root: %w", err)
	}
	g, err := newGH(ctx)
	if err != nil {
		return err
	}
	return cache(ctx, g, local, cacheOptions{apply: *apply, asJSON: *asJSON, deletePace: deletePace, out: out, errOut: errOut})
}

// deletePace separates deletes, which keeps a run that prunes many entries
// under GitHub's secondary rate limit.
const deletePace = 500 * time.Millisecond

type cacheOptions struct {
	apply, asJSON bool
	deletePace    time.Duration
	// out receives the report; errOut receives a line as each delete
	// happens.
	out, errOut io.Writer
}

// cache reports the repository's cache entries against policy and, with
// apply, deletes the entries outside it. Usage is what the listing holds less
// what this run deleted: a fresh listing or the usage endpoint lags the
// deletes. It returns errConflict, after reporting it, when the remaining
// usage exceeds the alert threshold or a family's budget.
func cache(ctx context.Context, src cacheSource, local []byte, opts cacheOptions) error {
	policy, err := cachepolicy.Parse(local)
	if err != nil {
		return fmt.Errorf("%s: %w", cachePolicyPath, err)
	}
	// Deleting by a policy the default branch does not hold would prune
	// what main's savers still write.
	if opts.apply {
		remote, err := src.defaultBranchPolicy(ctx)
		if err != nil {
			return fmt.Errorf("refusing --apply: read the default branch's %s: %w", cachePolicyPath, err)
		}
		if !bytes.Equal(local, remote) {
			return fmt.Errorf("refusing --apply: %s differs from the default branch's copy; run from a checkout of the default branch", cachePolicyPath)
		}
	}

	entries, err := src.caches(ctx)
	if err != nil {
		return err
	}
	report := cacheReport{
		Applied:     opts.apply,
		Entries:     classifyCaches(policy, entries),
		Deleted:     []int64{},
		Problems:    []string{},
		BudgetBytes: policy.BudgetBytes,
		AlertBytes:  policy.AlertBytes,
	}

	var deleteErrs []error
	deleted := map[int64]bool{}
	attempted := false
	for _, entry := range report.Entries {
		if entry.Class == cacheInPolicy || !opts.apply {
			continue
		}
		if attempted && !sleep(ctx, opts.deletePace) {
			deleteErrs = append(deleteErrs, ctx.Err())
			break
		}
		attempted = true
		if err := src.deleteCache(ctx, entry.ID); err != nil {
			fmt.Fprintf(opts.errOut, "delete failed %d %s: %v\n", entry.ID, entry.Key, err)
			deleteErrs = append(deleteErrs, fmt.Errorf("delete cache %d (%s): %w", entry.ID, entry.Key, err))
			continue
		}
		fmt.Fprintf(opts.errOut, "deleted %d %s\n", entry.ID, entry.Key)
		deleted[entry.ID] = true
		report.Deleted = append(report.Deleted, entry.ID)
	}

	usage := map[string]*cacheFamilyUsage{}
	keys := map[string]map[string]bool{}
	for _, family := range policy.Families {
		usage[family.Name] = &cacheFamilyUsage{Name: family.Name, BudgetBytes: family.BudgetBytes}
		keys[family.Name] = map[string]bool{}
	}
	for _, entry := range report.Entries {
		// Without apply, the entries that would remain are the in-policy ones.
		remains := !deleted[entry.ID] && (opts.apply || entry.Class == cacheInPolicy)
		if !remains {
			continue
		}
		report.TotalBytes += entry.Size
		if family := usage[entry.Family]; family != nil {
			family.Entries++
			family.Bytes += entry.Size
			keys[entry.Family][entry.Key] = true
		}
	}
	for _, family := range policy.Families {
		u := usage[family.Name]
		u.Keys = len(keys[family.Name])
		report.Families = append(report.Families, *u)
		if u.Bytes > u.BudgetBytes {
			report.Problems = append(report.Problems, fmt.Sprintf("family %s uses %s, over its %s budget", u.Name, formatBytes(u.Bytes), formatBytes(u.BudgetBytes)))
		}
	}
	if report.TotalBytes > policy.AlertBytes {
		report.Problems = append(report.Problems, fmt.Sprintf("caches use %s in total, over the %s alert threshold", formatBytes(report.TotalBytes), formatBytes(policy.AlertBytes)))
	}

	if opts.asJSON {
		if err := json.NewEncoder(opts.out).Encode(report); err != nil {
			return err
		}
	} else if err := report.writeText(opts.out); err != nil {
		return err
	}
	if len(deleteErrs) > 0 {
		return errors.Join(deleteErrs...)
	}
	if len(report.Problems) > 0 {
		return errConflict
	}
	return nil
}

// writeText prints Markdown, so the same text serves a terminal and a
// workflow job summary.
func (r cacheReport) writeText(out io.Writer) error {
	w := tabwriter.NewWriter(out, 0, 0, 1, ' ', 0)
	fmt.Fprintln(w, "| Family\t| Keys\t| Entries\t| Usage\t| Budget\t|")
	fmt.Fprintln(w, "| ---\t| ---:\t| ---:\t| ---:\t| ---:\t|")
	for _, f := range r.Families {
		fmt.Fprintf(w, "| %s\t| %d\t| %d\t| %s\t| %s\t|\n", f.Name, f.Keys, f.Entries, formatBytes(f.Bytes), formatBytes(f.BudgetBytes))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	verb := "Would remain"
	if r.Applied {
		verb = "Remaining"
	}
	fmt.Fprintf(out, "\n%s: %s of %s budget (alert at %s).\n", verb, formatBytes(r.TotalBytes), formatBytes(r.BudgetBytes), formatBytes(r.AlertBytes))

	var outside []classifiedCache
	for _, entry := range r.Entries {
		if entry.Class != cacheInPolicy {
			outside = append(outside, entry)
		}
	}
	if len(outside) > 0 {
		fmt.Fprint(out, "\nOutside policy:\n\n")
		w = tabwriter.NewWriter(out, 0, 0, 1, ' ', 0)
		fmt.Fprintln(w, "| Action\t| Class\t| Ref\t| Key\t| Size\t| Last accessed\t|")
		fmt.Fprintln(w, "| ---\t| ---\t| ---\t| ---\t| ---:\t| ---\t|")
		for _, entry := range outside {
			action := "would delete"
			switch {
			case slices.Contains(r.Deleted, entry.ID):
				action = "deleted"
			case r.Applied:
				action = "delete failed"
			}
			fmt.Fprintf(w, "| %s\t| %s\t| %s\t| `%s`\t| %s\t| %s\t|\n", action, entry.Class, entry.Ref, entry.Key,
				formatBytes(entry.Size), entry.LastAccessed.UTC().Format("2006-01-02 15:04Z"))
		}
		if err := w.Flush(); err != nil {
			return err
		}
	}

	if len(r.Problems) > 0 {
		fmt.Fprintln(out)
		for _, problem := range r.Problems {
			fmt.Fprintf(out, "- %s\n", problem)
		}
	}
	return nil
}

// formatBytes uses decimal units, as the policy's budgets do.
func formatBytes(n int64) string {
	switch {
	case n >= 1e9:
		return fmt.Sprintf("%.2f GB", float64(n)/1e9)
	case n >= 1e6:
		return fmt.Sprintf("%.1f MB", float64(n)/1e6)
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func (g *gh) caches(ctx context.Context) ([]cacheEntry, error) {
	raw, err := g.read(ctx, "--paginate", "--slurp", g.repoPath()+"/actions/caches?per_page=100")
	if err != nil {
		return nil, err
	}
	return decodeCaches(raw)
}

// decodeCaches reads a slurped, paginated cache listing. It refuses a listing
// it cannot trust to delete by: an entry missing what classification needs,
// or a count that differs from the API's total, which pages shifting under
// concurrent saves or deletes produce. An entry repeated across pages is
// counted once.
func decodeCaches(raw []byte) ([]cacheEntry, error) {
	var pages []struct {
		TotalCount int          `json:"total_count"`
		Caches     []cacheEntry `json:"actions_caches"`
	}
	if err := json.Unmarshal(raw, &pages); err != nil {
		return nil, fmt.Errorf("decode cache listing: %w", err)
	}
	if len(pages) == 0 {
		return nil, errors.New("decode cache listing: no pages")
	}

	var entries []cacheEntry
	seen := map[int64]bool{}
	for _, page := range pages {
		for _, entry := range page.Caches {
			switch {
			case entry.ID == 0:
				return nil, fmt.Errorf("cache listing has an entry without an id (key %q)", entry.Key)
			case entry.Ref == "", entry.Key == "":
				return nil, fmt.Errorf("cache listing entry %d lacks a ref or key", entry.ID)
			case entry.LastAccessed.IsZero():
				return nil, fmt.Errorf("cache listing entry %d lacks last_accessed_at", entry.ID)
			case seen[entry.ID]:
				continue
			}
			seen[entry.ID] = true
			entries = append(entries, entry)
		}
	}
	if total := pages[0].TotalCount; len(entries) != total {
		return nil, fmt.Errorf("cache listing holds %d entries but reports %d; it changed while paging, so run again", len(entries), total)
	}
	return entries, nil
}

func (g *gh) defaultBranchPolicy(ctx context.Context) ([]byte, error) {
	// Without a ref, the contents API reads the default branch.
	return g.read(ctx, "-H", "Accept: application/vnd.github.raw+json", g.repoPath()+"/contents/"+cachePolicyPath)
}

func (g *gh) deleteCache(ctx context.Context, id int64) error {
	// Deleting by id is safe to repeat, so it retries as a read does, and an
	// entry already gone answers 404 and counts as deleted.
	_, err := g.read(ctx, "-X", "DELETE", fmt.Sprintf("%s/actions/caches/%d", g.repoPath(), id))
	var failed *ghError
	if errors.As(err, &failed) && strings.Contains(failed.stderr, "(HTTP 404)") {
		return nil
	}
	return err
}
