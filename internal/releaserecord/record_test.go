package releaserecord_test

import (
	"errors"
	"testing"

	"github.com/katl-dev/katl/internal/releaserecord"
)

const digest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

var run = releaserecord.Run{Server: "https://github.com", Repository: "katl-dev/katl", ID: 9876543210, Attempt: 2}

const target = "https://github.com/katl-dev/katl/actions/runs/9876543210/attempts/2"

// cases pairs each phase of a candidate and a final with the exact status
// the design document specifies for it.
var cases = []struct {
	name   string
	record releaserecord.Record
	status releaserecord.Status
}{
	{
		name:   "candidate building",
		record: releaserecord.Record{Version: "2026.12.1-rc.3", Phase: releaserecord.Building, Run: run},
		status: releaserecord.Status{Context: "katl/release/v2026.12.1-rc.3", State: "pending", Description: "building", TargetURL: target},
	},
	{
		name:   "candidate failed",
		record: releaserecord.Record{Version: "2026.12.1-rc.3", Phase: releaserecord.Failed, Run: run},
		status: releaserecord.Status{Context: "katl/release/v2026.12.1-rc.3", State: "failure", Description: "failed", TargetURL: target},
	},
	{
		name:   "candidate qualified",
		record: releaserecord.Record{Version: "2026.12.1-rc.3", Phase: releaserecord.Qualified, Digest: digest, Run: run},
		status: releaserecord.Status{Context: "katl/release/v2026.12.1-rc.3", State: "success", Description: "qualified sha256:" + digest, TargetURL: target},
	},
	{
		name:   "final building",
		record: releaserecord.Record{Version: "2026.12.1", Phase: releaserecord.Building, Run: run},
		status: releaserecord.Status{Context: "katl/release/v2026.12.1", State: "pending", Description: "building", TargetURL: target},
	},
	{
		name:   "final failed",
		record: releaserecord.Record{Version: "2026.12.1", Phase: releaserecord.Failed, Run: run},
		status: releaserecord.Status{Context: "katl/release/v2026.12.1", State: "failure", Description: "failed", TargetURL: target},
	},
	{
		name:   "final qualified",
		record: releaserecord.Record{Version: "2026.12.1", Phase: releaserecord.Qualified, Digest: digest, Run: run},
		status: releaserecord.Status{Context: "katl/release/v2026.12.1", State: "success", Description: "qualified sha256:" + digest, TargetURL: target},
	},
}

func TestStatus(t *testing.T) {
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.record.Status()
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.status {
				t.Fatalf("Status() = %+v, want %+v", got, tc.status)
			}
		})
	}
}

func TestParse(t *testing.T) {
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := releaserecord.Parse(tc.status)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.record {
				t.Fatalf("Parse() = %+v, want %+v", got, tc.record)
			}
		})
	}

	t.Run("server with port", func(t *testing.T) {
		got, err := releaserecord.Parse(releaserecord.Status{
			Context: "katl/release/v2026.12.1", State: "pending", Description: "building",
			TargetURL: "https://ghe.example.test:8443/release/katl/actions/runs/7/attempts/1",
		})
		if err != nil {
			t.Fatal(err)
		}
		want := releaserecord.Run{Server: "https://ghe.example.test:8443", Repository: "release/katl", ID: 7, Attempt: 1}
		if got.Run != want {
			t.Fatalf("Run = %+v, want %+v", got.Run, want)
		}
	})
}

func TestParseRejects(t *testing.T) {
	valid := releaserecord.Status{Context: "katl/release/v2026.12.1", State: "success", Description: "qualified sha256:" + digest, TargetURL: target}
	for _, tc := range []struct {
		name   string
		change func(*releaserecord.Status)
	}{
		{"context without v", func(s *releaserecord.Status) { s.Context = "katl/release/2026.12.1" }},
		{"context without version", func(s *releaserecord.Status) { s.Context = "katl/release/v" }},

		{"qualified without digest", func(s *releaserecord.Status) { s.Description = "qualified" }},
		{"qualified with empty digest", func(s *releaserecord.Status) { s.Description = "qualified sha256:" }},
		{"bare digest", func(s *releaserecord.Status) { s.Description = "qualified " + digest }},
		{"other algorithm", func(s *releaserecord.Status) { s.Description = "qualified sha512:" + digest }},
		{"uppercase digest", func(s *releaserecord.Status) {
			s.Description = "qualified sha256:0123456789ABCDEF0123456789abcdef0123456789abcdef0123456789abcdef"
		}},
		{"short digest", func(s *releaserecord.Status) { s.Description = "qualified sha256:" + digest[1:] }},
		{"long digest", func(s *releaserecord.Status) { s.Description = "qualified sha256:" + digest + "0" }},
		{"doubled space", func(s *releaserecord.Status) { s.Description = "qualified  sha256:" + digest }},
		{"trailing text", func(s *releaserecord.Status) { s.Description = "qualified sha256:" + digest + " later" }},
		{"capitalised phase", func(s *releaserecord.Status) { s.State, s.Description = "pending", "Building" }},
		{"trailing space", func(s *releaserecord.Status) { s.State, s.Description = "failure", "failed " }},
		{"building with digest", func(s *releaserecord.Status) { s.State, s.Description = "pending", "building sha256:"+digest }},

		{"published phase", func(s *releaserecord.Status) { s.Description = "published" }},
		{"empty description", func(s *releaserecord.Status) { s.Description = "" }},

		{"qualified pending", func(s *releaserecord.Status) { s.State = "pending" }},
		{"building success", func(s *releaserecord.Status) { s.Description = "building" }},
		{"failed with error state", func(s *releaserecord.Status) { s.State, s.Description = "error", "failed" }},

		{"run without attempt", func(s *releaserecord.Status) {
			s.TargetURL = "https://github.com/katl-dev/katl/actions/runs/9876543210"
		}},
		{"job URL", func(s *releaserecord.Status) {
			s.TargetURL = "https://github.com/katl-dev/katl/actions/runs/9876543210/job/5"
		}},
		{"http", func(s *releaserecord.Status) {
			s.TargetURL = "http://github.com/katl-dev/katl/actions/runs/9876543210/attempts/2"
		}},
		{"no scheme", func(s *releaserecord.Status) {
			s.TargetURL = "github.com/katl-dev/katl/actions/runs/9876543210/attempts/2"
		}},
		{"credentials", func(s *releaserecord.Status) {
			s.TargetURL = "https://bot@github.com/katl-dev/katl/actions/runs/9876543210/attempts/2"
		}},
		{"query", func(s *releaserecord.Status) { s.TargetURL = target + "?check_suite_focus=true" }},
		{"fragment", func(s *releaserecord.Status) { s.TargetURL = target + "#summary" }},
		{"trailing slash", func(s *releaserecord.Status) { s.TargetURL = target + "/" }},
		{"server path", func(s *releaserecord.Status) {
			s.TargetURL = "https://github.com/x/katl-dev/katl/actions/runs/9876543210/attempts/2"
		}},
		{"escaped repository", func(s *releaserecord.Status) {
			s.TargetURL = "https://github.com/katl%2Ddev/katl/actions/runs/9876543210/attempts/2"
		}},
		{"leading zero", func(s *releaserecord.Status) {
			s.TargetURL = "https://github.com/katl-dev/katl/actions/runs/09876543210/attempts/2"
		}},
		{"signed attempt", func(s *releaserecord.Status) {
			s.TargetURL = "https://github.com/katl-dev/katl/actions/runs/9876543210/attempts/+2"
		}},
		{"attempt zero", func(s *releaserecord.Status) {
			s.TargetURL = "https://github.com/katl-dev/katl/actions/runs/9876543210/attempts/0"
		}},
		{"empty target", func(s *releaserecord.Status) { s.TargetURL = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status := valid
			tc.change(&status)
			record, err := releaserecord.Parse(status)
			if err == nil {
				t.Fatalf("Parse(%+v) = %+v, want an error", status, record)
			}
			if errors.Is(err, releaserecord.ErrNotRecord) {
				t.Fatalf("Parse(%+v) = %v, want a malformed record rather than none", status, err)
			}
		})
	}

	if _, err := releaserecord.Parse(valid); err != nil {
		t.Fatalf("the unchanged status must parse: %v", err)
	}
}

func TestParseNotRecord(t *testing.T) {
	for _, context := range []string{"ci/unit", "katl/driver-qualification/standard/v1:" + digest, "katl/releases/v2026.12.1", ""} {
		_, err := releaserecord.Parse(releaserecord.Status{Context: context, State: "success", Description: "building", TargetURL: target})
		if !errors.Is(err, releaserecord.ErrNotRecord) {
			t.Errorf("Parse(context %q) = %v, want ErrNotRecord", context, err)
		}
	}
}

func TestStatusRejects(t *testing.T) {
	for _, tc := range []struct {
		name   string
		record releaserecord.Record
	}{
		{"no version", releaserecord.Record{Phase: releaserecord.Building, Run: run}},
		{"qualified without digest", releaserecord.Record{Version: "2026.12.1", Phase: releaserecord.Qualified, Run: run}},
		{"qualified with prefixed digest", releaserecord.Record{Version: "2026.12.1", Phase: releaserecord.Qualified, Digest: "sha256:" + digest, Run: run}},
		{"failed with digest", releaserecord.Record{Version: "2026.12.1", Phase: releaserecord.Failed, Digest: digest, Run: run}},
		{"unknown phase", releaserecord.Record{Version: "2026.12.1", Phase: "published", Run: run}},
		{"no phase", releaserecord.Record{Version: "2026.12.1", Run: run}},
		{"server with slash", releaserecord.Record{Version: "2026.12.1", Phase: releaserecord.Building,
			Run: releaserecord.Run{Server: "https://github.com/", Repository: "katl-dev/katl", ID: 1, Attempt: 1}}},
		{"repository without owner", releaserecord.Record{Version: "2026.12.1", Phase: releaserecord.Building,
			Run: releaserecord.Run{Server: "https://github.com", Repository: "katl", ID: 1, Attempt: 1}}},
		{"no run", releaserecord.Record{Version: "2026.12.1", Phase: releaserecord.Building,
			Run: releaserecord.Run{Server: "https://github.com", Repository: "katl-dev/katl", Attempt: 1}}},
		{"no attempt", releaserecord.Record{Version: "2026.12.1", Phase: releaserecord.Building,
			Run: releaserecord.Run{Server: "https://github.com", Repository: "katl-dev/katl", ID: 1}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if status, err := tc.record.Status(); err == nil {
				t.Fatalf("Status() = %+v, want an error", status)
			}
		})
	}
}
