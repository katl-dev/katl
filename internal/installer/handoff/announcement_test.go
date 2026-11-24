package handoff

import "testing"

func TestParseAnnouncement(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		wantOK   bool
		wantErr  bool
		wantURL  string
		wantBase string
	}{
		{name: "unrelated line", line: "systemd[1]: Started foo."},
		{
			name:     "announcement",
			line:     "katlos-install waiting for config at http://192.168.122.44:8080/v1/config-bundle",
			wantOK:   true,
			wantURL:  "http://192.168.122.44:8080/v1/config-bundle",
			wantBase: "http://192.168.122.44:8080",
		},
		{
			name:     "IPv6 host with serial prefix and CRLF",
			line:     "[   12.3] katlos-install waiting for config at http://[fd00::44]:8080/v1/config-bundle\r",
			wantOK:   true,
			wantURL:  "http://[fd00::44]:8080/v1/config-bundle",
			wantBase: "http://[fd00::44]:8080",
		},
		{name: "wrong path", line: "katlos-install waiting for config at http://192.168.122.44:8080/config-bundle", wantErr: true},
		{name: "no path", line: "katlos-install waiting for config at http://192.168.122.44:8080", wantErr: true},
		{name: "truncated port", line: "katlos-install waiting for config at http://192.168.122.44:8", wantErr: true},
		{name: "no host", line: "katlos-install waiting for config at /v1/config-bundle", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok, err := ParseAnnouncement(test.line)
			if (err != nil) != test.wantErr || ok != test.wantOK {
				t.Fatalf("ok = %v, err = %v; want ok %v, error %v", ok, err, test.wantOK, test.wantErr)
			}
			if got.URL != test.wantURL || got.Base != test.wantBase {
				t.Fatalf("URL = %q, Base = %q; want %q, %q", got.URL, got.Base, test.wantURL, test.wantBase)
			}
		})
	}
}

func TestParseAnnouncementAcceptsAnnouncement(t *testing.T) {
	line := NewHandoffServer(nil).Announcement("http://10.0.2.15:8080/")
	got, ok, err := ParseAnnouncement(line)
	if err != nil || !ok || got.Base != "http://10.0.2.15:8080" {
		t.Fatalf("ParseAnnouncement(%q) = %#v, %v, %v", line, got, ok, err)
	}
}
