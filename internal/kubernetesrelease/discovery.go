package kubernetesrelease

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

type upstreamRelease struct {
	Tag        string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
}

// Discover selects the newest stable patch in each of the three newest minor
// branches, like the upstream support window. Existing artifacts are retained.
func Discover(ctx context.Context, client *http.Client, endpoint, token string) ([]string, error) {
	var releases []upstreamRelease
	for page := 1; ; page++ {
		if page > 20 {
			return nil, fmt.Errorf("upstream release listing exceeded 20 pages")
		}
		request, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s?per_page=100&page=%d", endpoint, page), nil)
		if err != nil {
			return nil, err
		}
		request.Header.Set("Accept", "application/vnd.github+json")
		request.Header.Set("User-Agent", "katl-kubernetes-release")
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response, err := client.Do(request)
		if err != nil {
			return nil, fmt.Errorf("discover Kubernetes releases: %w", err)
		}
		data, readErr := io.ReadAll(io.LimitReader(response.Body, 8<<20))
		response.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		if response.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("discover Kubernetes releases: HTTP %d", response.StatusCode)
		}
		var batch []upstreamRelease
		if err := json.Unmarshal(data, &batch); err != nil {
			return nil, err
		}
		releases = append(releases, batch...)
		if !strings.Contains(response.Header.Get("Link"), `rel="next"`) {
			break
		}
	}
	return newestReleases(releases)
}

func newestReleases(releases []upstreamRelease) ([]string, error) {
	latest := map[int][3]int{}
	for _, release := range releases {
		if release.Draft || release.Prerelease {
			continue
		}
		version, err := parseVersion(release.Tag)
		if err != nil || version[0] != 1 {
			continue
		}
		previous, exists := latest[version[1]]
		if !exists || compareVersions(version, previous) > 0 {
			latest[version[1]] = version
		}
	}
	if len(latest) == 0 {
		return nil, fmt.Errorf("upstream returned no stable Kubernetes releases")
	}
	minors := make([]int, 0, len(latest))
	for minor := range latest {
		minors = append(minors, minor)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(minors)))
	if len(minors) > 3 {
		minors = minors[:3]
	}
	var versions []string
	for _, minor := range minors {
		v := latest[minor]
		versions = append(versions, fmt.Sprintf("v%d.%d.%d", v[0], v[1], v[2]))
	}
	return versions, nil
}
