package handoff

import (
	"fmt"
	neturl "net/url"
	"strings"
)

const (
	// AnnouncementPrefix starts the serial line on which the installer announces
	// its config-bundle endpoint.
	AnnouncementPrefix = "katlos-install waiting for config at "

	configBundlePath = "/v1/config-bundle"
)

// Endpoint is the handoff endpoint named by an installer announcement.
type Endpoint struct {
	// Line is the announcement line, trimmed.
	Line string
	// URL is the config-bundle endpoint.
	URL string
	// Base is URL without the config-bundle path.
	Base string
}

// ParseAnnouncement parses one complete announcement line. ok is false when the
// line is not an announcement; an announcement whose URL does not end in the
// config-bundle path is an error.
func ParseAnnouncement(line string) (endpoint Endpoint, ok bool, err error) {
	line = strings.TrimSpace(line)
	start := strings.Index(line, AnnouncementPrefix)
	if start < 0 {
		return Endpoint{}, false, nil
	}

	raw := strings.TrimSpace(line[start+len(AnnouncementPrefix):])
	parsed, err := neturl.Parse(raw)
	if err != nil || parsed.Host == "" || !strings.HasSuffix(parsed.Path, configBundlePath) {
		return Endpoint{}, false, fmt.Errorf("handoff announcement %q does not name a URL ending in %s", line, configBundlePath)
	}
	parsed.Path = strings.TrimSuffix(parsed.Path, configBundlePath)
	return Endpoint{Line: line, URL: raw, Base: parsed.String()}, true, nil
}
