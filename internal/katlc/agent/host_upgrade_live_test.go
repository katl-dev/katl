package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/installer/generation"
)

func TestHostUpgradeAfterLivePromotion(t *testing.T) {
	for _, tc := range []struct {
		name        string
		changedRoot bool
	}{{"same runtime", false}, {"different runtime", true}} {
		t.Run(tc.name, func(t *testing.T) {
			server := newTestServer(t)
			writeKnownGoodHostUpgradeSource(t, server.Root)
			spec, _, err := generation.ReadGeneration(server.Root, "generation-0")
			if err != nil {
				t.Fatal(err)
			}
			spec.GenerationID = "live-config"
			spec.PreviousGenerationID = "generation-0"
			spec.Boot.LoaderEntryPath = "loader/entries/katl-live-config.conf"
			if tc.changedRoot {
				spec.Root.RuntimeArtifactSHA256 = strings.Repeat("b", 64)
			}
			state, err := generation.NewGenerationStatus(spec, generation.CommitStateCandidate, generation.BootStatePending, generation.HealthStateUnknown, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if err := generation.WriteGeneration(server.Root, spec, state); err != nil {
				t.Fatal(err)
			}
			if err := generation.PromoteLiveGeneration(generation.LivePromotionRequest{Root: server.Root, GenerationID: spec.GenerationID, Now: time.Now(), SetBootDefault: func(string, string) error { return nil }}); err != nil {
				t.Fatal(err)
			}

			request := hostUpgradeSubmitRequest("after-live")
			request.DryRun = true
			request.ExpectedCurrentGenerationId = "live-config"
			_, err = server.SubmitOperation(context.Background(), request)
			if (err != nil) != tc.changedRoot {
				t.Fatalf("upgrade error=%v, changed runtime=%v", err, tc.changedRoot)
			}
		})
	}
}
