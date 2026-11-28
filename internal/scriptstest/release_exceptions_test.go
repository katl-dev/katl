package scriptstest

import "github.com/katl-dev/katl/internal/releasestate"

const (
	artifactsWorkflowPath    = ".github/workflows/release-artifacts.yml"
	productionWorkflowPath   = ".github/workflows/" + productionBuild
	productionVMWorkflowPath = ".github/workflows/production-vmtests.yml"
	hostedVMWorkflowPath     = ".github/workflows/hosted-vmtests.yml"
)

// Each row exempts only the named obligations at one site. A gap expires
// when its obligation is covered or removed. #780 owns rehearsal evidence
// for the operations that need a real GitHub release or attestation.
var releaseExceptions = []releaseException{
	// Hosted setup and the alternate ARC runner in the regression graph.
	{workflowSite{".github/actions/hosted-vmtest/action.yml", "(composite)", "Prepare hermetic runner and fixture tools on ARC"}, "gap", 989, "", []string{"if inputs.prepared"}},
	{workflowSite{".github/actions/hosted-vmtest/action.yml", "(composite)", "Provision libvirt and device access"}, "gap", 989, "", []string{"shell"}},
	{workflowSite{".github/actions/hosted-vmtest/action.yml", "(composite)", "Record host"}, "gap", 989, "", []string{"shell"}},
	{workflowSite{".github/actions/hosted-vmtest/action.yml", "(composite)", "Verify host capabilities"}, "gap", 989, "", []string{"shell"}},

	// Shared build orchestration in the graph that #935 consolidates.
	{workflowSite{".github/actions/image-caches/action.yml", "(composite)", "Resolve RPM cache inputs"}, "gap", 935, "", []string{"shell"}},
	{workflowSite{".github/actions/image-caches/action.yml", "(composite)", "Resolve cache paths"}, "gap", 935, "", []string{"shell"}},
	{workflowSite{".github/actions/image-caches/action.yml", "(composite)", "Resolve cache week"}, "gap", 935, "", []string{"shell"}},

	{workflowSite{".github/actions/mkosi-builder/action.yml", "(composite)", "Configure build downloads"}, "gap", 935, "", []string{"shell"}},
	{workflowSite{".github/actions/mkosi-builder/action.yml", "(composite)", "builder"}, "gap", 935, "", []string{"shell"}},

	// The release App credential needs the release environment's App secret.
	{workflowSite{".github/actions/release-credential/action.yml", "(composite)", "Mint the katl-release token"}, "credential", 0, releaseRehearsal, []string{"reach"}},
	{workflowSite{".github/actions/release-credential/action.yml", "(composite)", "Mint the katl-release token"}, "credential", 0, "TestReleaseCredentialScope", []string{
		"with.app-id ${{ inputs.app-id }}",
		"with.private-key ${{ inputs.private-key }}",
		"with.repositories ${{ github.event.repository.name }}",
	}},
	{workflowSite{".github/actions/release-credential/action.yml", "(composite)", "Verify installation scope and repository access"}, "credential", 0, "TestReleaseCredentialScope", []string{"reach", "shell"}},
	{workflowSite{".github/actions/release-credential/action.yml", "(composite)", "Verify the release App slug"}, "credential", 0, "TestReleaseCredentialSlug", []string{"reach", "shell"}},

	{workflowSite{".github/actions/setup-go/action.yml", "(composite)", "Resolve Go cache"}, "gap", 935, "", []string{"shell"}},

	{workflowSite{".github/actions/setup-tools/action.yml", "(composite)", "Realise the ci shell"}, "gap", 989, "", []string{"shell"}},

	// The release call runs the old standalone VM path without the CI plan.
	{workflowSite{hostedVMWorkflowPath, "(on)", "workflow_dispatch"}, "gap", 989, "", []string{"trigger"}},
	{workflowSite{hostedVMWorkflowPath, "(on)", "workflow_dispatch.flavour"}, "gap", 989, "", []string{"input"}},
	{workflowSite{hostedVMWorkflowPath, "(on)", "workflow_dispatch.ref"}, "gap", 989, "", []string{"input"}},
	{workflowSite{hostedVMWorkflowPath, "(on)", "workflow_dispatch.runner"}, "gap", 989, "", []string{"input"}},
	{workflowSite{hostedVMWorkflowPath, "(workflow)", "(workflow)"}, "gap", 989, "", []string{
		"concurrency.group github.event.pull_request.number github.event_name github.ref github.workflow inputs.flavour",
	}},
	{workflowSite{hostedVMWorkflowPath, "images", "./_build/workflow-actions/.github/actions/image-caches #1"}, "gap", 989, "", []string{"if inputs.ref"}},
	{workflowSite{hostedVMWorkflowPath, "images", "./_build/workflow-actions/.github/actions/mkosi-builder #1"}, "gap", 989, "", []string{"if inputs.ref"}},
	{workflowSite{hostedVMWorkflowPath, "images", "./_build/workflow-actions/.github/actions/setup-go #1"}, "gap", 989, "", []string{"if inputs.ref"}},
	{workflowSite{hostedVMWorkflowPath, "images", "Build shared installer and lifecycle inputs"}, "gap", 989, "", []string{"shell"}},
	{workflowSite{hostedVMWorkflowPath, "images", "Record builder identity"}, "gap", 989, "", []string{"shell"}},
	{workflowSite{hostedVMWorkflowPath, "images", "Resolve Kubernetes candidate"}, "gap", 989, "", []string{"if inputs.kubernetes-version"}},
	{workflowSite{hostedVMWorkflowPath, "images", "Save container Go cache"}, "gap", 937, "", []string{"if github.event_name github.ref"}},
	{workflowSite{hostedVMWorkflowPath, "images", "Save mkosi RPM cache"}, "gap", 937, "", []string{"if github.event_name github.ref"}},
	{workflowSite{hostedVMWorkflowPath, "images", "Verify and stage Kubernetes bundle"}, "gap", 989, "", []string{"shell"}},
	{workflowSite{hostedVMWorkflowPath, "images", "actions/checkout@v7 #2"}, "gap", 989, "", []string{"if inputs.ref"}},
	{workflowSite{hostedVMWorkflowPath, "plan", "./.github/actions/setup-go #1"}, "gap", 989, "", []string{"if inputs.ref inputs.scenarios"}},
	{workflowSite{hostedVMWorkflowPath, "plan", "./_build/workflow-actions/.github/actions/setup-go #1"}, "gap", 989, "", []string{"if inputs.ref inputs.scenarios"}},
	{workflowSite{hostedVMWorkflowPath, "plan", "Select VM groups"}, "gap", 989, "", []string{"env.SCENARIOS ${{ inputs.scenarios }}", "reach", "shell"}},
	{workflowSite{hostedVMWorkflowPath, "plan", "Select VM runner"}, "gap", 989, "", []string{
		"run-reference GITHUB_EVENT_NAME",
		"run-reference GITHUB_REF",
		"shell",
	}},
	{workflowSite{hostedVMWorkflowPath, "plan", "actions/checkout@v7 #2"}, "gap", 989, "", []string{"if inputs.ref inputs.scenarios"}},
	{workflowSite{hostedVMWorkflowPath, "scenarios", "./_build/workflow-actions/.github/actions/setup-go #1"}, "gap", 989, "", []string{"if inputs.ref"}},
	{workflowSite{hostedVMWorkflowPath, "scenarios", "Derive component and proof inputs from the install image"}, "gap", 989, "", []string{"shell"}},
	{workflowSite{hostedVMWorkflowPath, "scenarios", "Download VM inputs"}, "gap", 989, "", []string{"shell"}},
	{workflowSite{hostedVMWorkflowPath, "scenarios", "Run existing scenarios"}, "gap", 989, "", []string{"shell"}},
	{workflowSite{hostedVMWorkflowPath, "scenarios", "Verify and import producer inputs"}, "gap", 989, "", []string{"shell"}},
	{workflowSite{hostedVMWorkflowPath, "scenarios", "actions/checkout@v7 #2"}, "gap", 989, "", []string{"if inputs.ref"}},

	// The production release call still selects release versions.
	{workflowSite{productionWorkflowPath, "(workflow)", "(workflow)"}, "gap", 992, "TestKatlReleaseArtifactVersion", []string{"env.KATL_VERSION ${{ inputs.version }}"}},
	{workflowSite{productionWorkflowPath, "assemble", "Assemble and verify installer ISO"}, "gap", 935, "", []string{"shell"}},
	{workflowSite{productionWorkflowPath, "assemble", "Build katlctl"}, "gap", 935, "", []string{"shell"}},
	{workflowSite{productionWorkflowPath, "assemble", "Download release extension closures"}, "gap", 935, "", []string{"shell"}},
	{workflowSite{productionWorkflowPath, "assemble", "Package install and upgrade images concurrently"}, "gap", 935, "", []string{"shell"}},
	{workflowSite{productionWorkflowPath, "assemble", "Record disk margin"}, "gap", 935, "", []string{"shell"}},
	{workflowSite{productionWorkflowPath, "assemble", "Resolve artifact names"}, "gap", 935, "", []string{"shell"}},
	{workflowSite{productionWorkflowPath, "assemble", "Stage supported publication artifacts"}, "gap", 992, "TestKatlReleaseArtifactStage, TestKatlReleaseArtifactNotes, TestKatlReleaseArtifactNotesSiblingRelease, TestKatlReleaseArtifactNotesAfterPromotion", []string{"version"}},
	{workflowSite{productionWorkflowPath, "assemble", "Validate flavour"}, "gap", 935, "", []string{"shell"}},
	{workflowSite{productionWorkflowPath, "assemble", "Verify KatlOS images"}, "gap", 935, "", []string{"shell"}},
	{workflowSite{productionWorkflowPath, "extensions", "Validate flavour"}, "gap", 935, "", []string{"shell"}},
	{workflowSite{productionWorkflowPath, "installer", "Validate flavour"}, "gap", 935, "", []string{"shell"}},
	{workflowSite{productionWorkflowPath, "installer", "Verify installer"}, "gap", 935, "", []string{"shell"}},
	{workflowSite{productionWorkflowPath, "runtime", "Inventory release extensions"}, "gap", 935, "", []string{"shell"}},
	{workflowSite{productionWorkflowPath, "runtime", "Resolve mkosi version"}, "gap", 935, "", []string{"shell"}},
	{workflowSite{productionWorkflowPath, "runtime", "Validate flavour"}, "gap", 935, "", []string{"shell"}},
	{workflowSite{productionWorkflowPath, "runtime", "Verify runtime"}, "gap", 935, "", []string{"shell"}},

	// Production VM retains the legacy manual qualification interface.
	{workflowSite{productionVMWorkflowPath, "(on)", "workflow_dispatch"}, "gap", 907, "", []string{"trigger"}},
	{workflowSite{productionVMWorkflowPath, "(on)", "workflow_dispatch.artifact-name"}, "gap", 907, "", []string{"input"}},
	{workflowSite{productionVMWorkflowPath, "(on)", "workflow_dispatch.flavour"}, "gap", 907, "", []string{"input"}},
	{workflowSite{productionVMWorkflowPath, "(on)", "workflow_dispatch.previous-tag"}, "gap", 907, "", []string{"input"}},
	{workflowSite{productionVMWorkflowPath, "(on)", "workflow_dispatch.run-id"}, "gap", 907, "", []string{"input"}},
	{workflowSite{productionVMWorkflowPath, "qualification", "Verify candidate and previous release checksums"}, "gap", 935, "", []string{"shell"}},

	// Legacy triggers and steps, and the as-yet unshared called mode.
	{workflowSite{artifactsWorkflowPath, "(on)", "schedule"}, "gap", 937, "", []string{"trigger"}},
	{workflowSite{artifactsWorkflowPath, "(on)", "workflow_dispatch"}, "gap", 907, "", []string{"trigger"}},
	{workflowSite{artifactsWorkflowPath, "(on)", "workflow_dispatch.version"}, "gap", 907, "", []string{"input"}},
	{workflowSite{artifactsWorkflowPath, "(workflow)", "(workflow)"}, "gap", 935, "", []string{
		"concurrency.group github.event_name github.workflow inputs.commit",
	}},
	{workflowSite{artifactsWorkflowPath, "build-lts", "(job)"}, "gap", 935, "", []string{"reach"}},
	{workflowSite{artifactsWorkflowPath, "build-standard", "(job)"}, "gap", 935, "", []string{"reach"}},
	{workflowSite{artifactsWorkflowPath, "bundle", "Attest staged release artifacts"}, "external write", 0, releaseRehearsal, []string{"reach"}},
	{workflowSite{artifactsWorkflowPath, "bundle", "Attest staged release artifacts"}, "gap", 998, "", []string{"if inputs.commit"}},
	{workflowSite{artifactsWorkflowPath, "bundle", "Digest release checksums"}, "gap", 935, "", []string{
		"if inputs.commit",
		"reach",
		"shell",
	}},
	{workflowSite{artifactsWorkflowPath, "bundle", "actions/upload-artifact@v7 #1"}, "gap", 935, "", []string{"reach"}},
	{workflowSite{artifactsWorkflowPath, "identity", "Resolve release identity"}, "gap", 935, "", []string{
		"env.CALLED_COMMIT ${{ inputs.commit }}",
		"reach",
		"run-reference GITHUB_EVENT_NAME",
		"run-reference GITHUB_REF_NAME",
		"run-reference GITHUB_REF_TYPE",
		"shell",
	}},
	{workflowSite{artifactsWorkflowPath, "identity", "Resolve release identity"}, "gap", 992, "TestKatlReleaseArtifactVersion", []string{"env.MANUAL_VERSION ${{ inputs.version }}", "version"}},
	{workflowSite{artifactsWorkflowPath, "identity", "Resolve supported upgrade source"}, "gap", 935, "", []string{"reach", "shell"}},
	{workflowSite{artifactsWorkflowPath, "qualified-bytes", " #1"}, "gap", 935, "", []string{"reach"}},
	{workflowSite{artifactsWorkflowPath, "qualified-bytes", "Download qualified checksums"}, "gap", 935, "", []string{"reach"}},
	{workflowSite{artifactsWorkflowPath, "qualified-bytes", "Download the bundle"}, "gap", 935, "", []string{"reach"}},
	{workflowSite{artifactsWorkflowPath, "release-validation", "verdict"}, "gap", 989, "", []string{"reach", "shell"}},
	{workflowSite{artifactsWorkflowPath, "unit", "Run fast checks"}, "gap", 989, "", []string{"reach"}},
	{workflowSite{artifactsWorkflowPath, "vmtests", "(job)"}, "gap", 989, "", []string{"reach"}},

	// Release orchestration and operations that require real release authority.
	{workflowSite{releasestate.ReleaseWorkflow, "(workflow)", "(workflow)"}, "gap", 991, "", []string{"concurrency.group github.ref"}},
	{workflowSite{releasestate.ReleaseWorkflow, "build", "(job)"}, "gap", 935, "", []string{"call"}},
	{workflowSite{releasestate.ReleaseWorkflow, "build", "(job)"}, "gap", 992, "TestKatlReleaseArtifactVersion", []string{"reach"}},
	{workflowSite{releasestate.ReleaseWorkflow, "decide", "Decide the release action"}, "gap", 991, "", []string{
		"env.RETRY ${{ inputs.retry }}",
		"reach",
		"shell",
	}},
	{workflowSite{releasestate.ReleaseWorkflow, "publish", "./.github/actions/setup-go #1"}, "gap", 991, "", []string{"reach"}},
	{workflowSite{releasestate.ReleaseWorkflow, "publish", "actions/checkout@v7 #1"}, "gap", 991, "", []string{"reach"}},
	{workflowSite{releasestate.ReleaseWorkflow, "publish", "(environment)"}, "approval", 0, releaseRehearsal, []string{"reach"}},
	{workflowSite{releasestate.ReleaseWorkflow, "publish", "Authenticate registry publication"}, "credential", 0, releaseRehearsal, []string{"reach", "run-reference GITHUB_ACTOR", "shell"}},
	{workflowSite{releasestate.ReleaseWorkflow, "publish", "Compare extension inputs with the bundle"}, "gap", 991, "", []string{"reach", "shell"}},
	{workflowSite{releasestate.ReleaseWorkflow, "publish", "Confirm the bytes qualified by Production VM"}, "gap", 991, "", []string{"reach"}},
	{workflowSite{releasestate.ReleaseWorkflow, "publish", "Confirm the final decision after approval"}, "approval", 0, "TestConfirmFinal", []string{"reach"}},
	{workflowSite{releasestate.ReleaseWorkflow, "publish", "Confirm the qualified bundle digest"}, "gap", 991, "", []string{"reach", "shell"}},
	{workflowSite{releasestate.ReleaseWorkflow, "publish", "Download both qualification records"}, "gap", 991, "", []string{"reach"}},
	{workflowSite{releasestate.ReleaseWorkflow, "publish", "Download final extension publication inputs"}, "gap", 991, "", []string{"reach"}},
	{workflowSite{releasestate.ReleaseWorkflow, "publish", "Download the qualified bundle"}, "gap", 991, "", []string{"reach"}},
	{workflowSite{releasestate.ReleaseWorkflow, "publish", "Draft the final release"}, "external write", 0, "TestDraftRelease", []string{"reach"}},
	{workflowSite{releasestate.ReleaseWorkflow, "publish", "Publish qualified extension bundles"}, "external write", 0, releaseRehearsal, []string{"reach", "shell"}},
	{workflowSite{releasestate.ReleaseWorkflow, "publish", "Publish the final release and tag"}, "external write", 0, "TestPublishRelease", []string{"reach"}},
	{workflowSite{releasestate.ReleaseWorkflow, "publish", "Require a decision from this attempt"}, "gap", 991, "", []string{"reach", "shell"}},
	{workflowSite{releasestate.ReleaseWorkflow, "publish", "Resolve the bundle name"}, "gap", 991, "", []string{"reach", "shell"}},
	{workflowSite{releasestate.ReleaseWorkflow, "publish", "Resolve the bundle name"}, "gap", 992, "TestKatlReleaseArtifactVersion", []string{"version"}},
	{workflowSite{releasestate.ReleaseWorkflow, "publish", "Verify the publishing credential"}, "credential", 0, releaseRehearsal, []string{"reach"}},
	{workflowSite{releasestate.ReleaseWorkflow, "publish", "Summarize the publishing credential"}, "gap", 991, "", []string{"reach", "shell"}},
	{workflowSite{releasestate.ReleaseWorkflow, "publish-candidate", "./.github/actions/setup-go #1"}, "gap", 991, "", []string{"reach"}},
	{workflowSite{releasestate.ReleaseWorkflow, "publish-candidate", "actions/checkout@v7 #1"}, "gap", 991, "", []string{"reach"}},
	{workflowSite{releasestate.ReleaseWorkflow, "publish-candidate", "Authenticate registry publication"}, "credential", 0, releaseRehearsal, []string{"reach", "run-reference GITHUB_ACTOR", "shell"}},
	{workflowSite{releasestate.ReleaseWorkflow, "publish-candidate", "Compare extension inputs with the bundle"}, "gap", 991, "", []string{"reach", "shell"}},
	{workflowSite{releasestate.ReleaseWorkflow, "publish-candidate", "Confirm the qualified bundle digest"}, "gap", 991, "", []string{"reach", "shell"}},
	{workflowSite{releasestate.ReleaseWorkflow, "publish-candidate", "Download candidate extension publication inputs"}, "gap", 991, "", []string{"reach"}},
	{workflowSite{releasestate.ReleaseWorkflow, "publish-candidate", "Download recorded LTS extension publication inputs"}, "gap", 991, "", []string{"reach"}},
	{workflowSite{releasestate.ReleaseWorkflow, "publish-candidate", "Download recorded standard extension publication inputs"}, "gap", 991, "", []string{"reach"}},
	{workflowSite{releasestate.ReleaseWorkflow, "publish-candidate", "Download the candidate bundle"}, "gap", 991, "", []string{"reach"}},
	{workflowSite{releasestate.ReleaseWorkflow, "publish-candidate", "Download the recorded candidate bundle"}, "gap", 991, "", []string{"reach"}},
	{workflowSite{releasestate.ReleaseWorkflow, "publish-candidate", "Draft the pre-release"}, "external write", 0, "TestDraftRelease", []string{"reach"}},
	{workflowSite{releasestate.ReleaseWorkflow, "publish-candidate", "Find artifacts of the recorded build attempt"}, "gap", 991, "", []string{"reach", "shell"}},
	{workflowSite{releasestate.ReleaseWorkflow, "publish-candidate", "Publish qualified extension bundles"}, "external write", 0, releaseRehearsal, []string{"reach", "shell"}},
	{workflowSite{releasestate.ReleaseWorkflow, "publish-candidate", "Publish the pre-release"}, "external write", 0, "TestPublishRelease", []string{"reach"}},
	{workflowSite{releasestate.ReleaseWorkflow, "publish-candidate", "Require a decision from this attempt"}, "gap", 991, "", []string{"reach", "shell"}},
	{workflowSite{releasestate.ReleaseWorkflow, "publish-candidate", "Require a release candidate"}, "gap", 991, "", []string{"reach"}},
	{workflowSite{releasestate.ReleaseWorkflow, "publish-candidate", "Resolve the bundle name"}, "gap", 991, "", []string{"reach", "shell"}},
	{workflowSite{releasestate.ReleaseWorkflow, "publish-candidate", "Resolve the bundle name"}, "gap", 992, "TestKatlReleaseArtifactVersion", []string{"version"}},
	{workflowSite{releasestate.ReleaseWorkflow, "qualify", "./.github/actions/setup-go #1"}, "gap", 991, "", []string{"reach"}},
	{workflowSite{releasestate.ReleaseWorkflow, "qualify", "actions/checkout@v7 #1"}, "gap", 991, "", []string{"reach"}},
	{workflowSite{releasestate.ReleaseWorkflow, "qualify", "Download the validated bundle"}, "gap", 991, "", []string{"reach"}},
	{workflowSite{releasestate.ReleaseWorkflow, "qualify", "Record failure before qualification"}, "external write", 0, "TestRecord", []string{"reach"}},
	{workflowSite{releasestate.ReleaseWorkflow, "qualify", "Require a decision from this attempt"}, "gap", 991, "", []string{"reach", "shell"}},
	{workflowSite{releasestate.ReleaseWorkflow, "qualify", "Require a validated bundle"}, "gap", 991, "", []string{"reach", "shell"}},
	{workflowSite{releasestate.ReleaseWorkflow, "qualify", "Require a validated bundle"}, "gap", 992, "TestKatlReleaseArtifactVersion", []string{"version"}},
	{workflowSite{releasestate.ReleaseWorkflow, "qualify", "Verify, summarize and record qualification"}, "external write", 0, releaseRehearsal, []string{"reach", "shell"}},
	{workflowSite{releasestate.ReleaseWorkflow, "start", "./.github/actions/setup-go #1"}, "gap", 991, "", []string{"reach"}},
	{workflowSite{releasestate.ReleaseWorkflow, "start", "actions/checkout@v7 #1"}, "gap", 991, "", []string{"reach"}},
	{workflowSite{releasestate.ReleaseWorkflow, "start", "Create the candidate tag"}, "external write", 0, "TestReleaseCandidateTag", []string{"reach", "shell"}},
	{workflowSite{releasestate.ReleaseWorkflow, "start", "Record the start of the build"}, "external write", 0, "TestRecord", []string{"reach"}},
	{workflowSite{releasestate.ReleaseWorkflow, "start", "Require a decision from this attempt"}, "gap", 991, "", []string{"reach", "shell"}},
}
