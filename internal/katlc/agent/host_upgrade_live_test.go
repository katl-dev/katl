package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/katl-dev/katl/internal/generation"
)

func TestHostUpgradeAfterLivePromotion(t *testing.T) {
	for _, tc := range []struct {
		name        string
		changedRoot bool
	}{{"same runtime", false}, {"different runtime", true}} {
		t.Run(tc.name, func(t *testing.T) {
			server := newTestServer(t)
			writeKnownGoodHostUpgradeSource(t, server)
			spec, _, err := generation.ReadGeneration(server.Root, "generation-0")
			if err != nil {
				t.Fatal(err)
			}
			spec.GenerationID = "live-config"
			spec.Confexts[0].Path = "/var/lib/katl/generations/live-config/confext"
			spec.PreviousGenerationID = "generation-0"
			spec.Boot.LoaderEntryPath = "loader/entries/katl-live-config.conf"
			if tc.changedRoot {
				spec.Root.RuntimeArtifactSHA256 = strings.Repeat("b", 64)
			}
			state, err := generation.NewGenerationStatus(spec, generation.CommitStateCandidate, generation.BootStatePending, generation.HealthStateUnknown, time.Now())
			if tc.changedRoot {
				state, err = generation.NewGenerationStatus(spec, generation.CommitStateCommitted, generation.BootStateGood, generation.HealthStateHealthy, time.Now())
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := generation.WriteGeneration(server.Root, spec, state); err != nil {
				t.Fatal(err)
			}
			writeTestBootArtifacts(t, server.Root, spec)
			if tc.changedRoot {
				// The owner never promotes live a candidate that boots another
				// runtime, so write the selection such a promotion would publish.
				selection, err := generation.ReadBootSelection(server.Root)
				if err != nil {
					t.Fatal(err)
				}
				selection.DefaultGenerationID, selection.ActiveGenerationID = spec.GenerationID, spec.GenerationID
				selection.DefaultBootEntry = spec.Boot.LoaderEntryPath
				if err := generation.WriteBootSelection(server.Root, selection); err != nil {
					t.Fatal(err)
				}
			} else if err := server.BootMover.PromoteLive(generation.LivePromotionRequest{GenerationID: spec.GenerationID, OperationID: "promote-live-config", Now: time.Now()}); err != nil {
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
