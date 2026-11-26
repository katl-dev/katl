# Management trust transition

A trusted-network node moves to mTLS management through a confirm-or-revert
trial, so a mistake in the new credentials cannot lock the operator out. The
public driver is `katlctl management enable-mtls` (#672). Rotation and recovery
use the same listener trust report to verify an installed mTLS authority.

## Trial lifecycle

`BeginManagementTransition` stages the replacement credentials under
`versions/<authority>`, arms a transient systemd timer that runs
`katlc agent management-trial revert`, and then records a live trial in
`transition.json`. Arming comes before the record, so every persisted live
trial has a revert scheduled. A begin that fails after staging leaves an
unreferenced version; the next begin discards it before staging, so material
reissued under the same authority is accepted.

While the trial is live, katlc-agent serves only the staged credentials until
the trial's CLOCK_BOOTTIME deadline in the boot that began it.
`ConfirmManagementTransition` is accepted only over TLS with a client
certificate from the new authority. It points `active` at the new version and
publishes the mode file last, so the mode file is the commit point.

`nodeidentity.RevertManagementTrial` is the only revert implementation. The
timer, agent start, a late confirm, the agent's own deadline watch and the
console command all call it. Each revert is scoped to a trial ID and runs at
most once. An unreadable record is moved aside instead, because the mode file
and `active` alone decide the installed mode.

## One owner for the served credentials

The agent's listener loop is the only owner of which credentials it serves.
It re-reads management state and rebinds the same address in process when a
trial begins, is reverted or reaches its deadline. On-node commands that
change management state, the trial revert and `katlc agent rotate-management`,
run `systemctl reload-or-restart katlc-agent.service`. The unit is
`Type=notify-reload`, so systemd sends SIGHUP and waits until the agent
reports `READY=1` from the new listener. Each re-serve ends with
`RELOADING=1`, `READY=1` and a `MONOTONIC_USEC` read after the loop absorbed
pending signals, so a SIGHUP that arrives while the loop is already
re-serving for an in-process change still ends the reload systemd began.

A management change restarts katlc-agent only when the agent is not running,
because `reload-or-restart` then starts it. Even then, and when systemd
restarts a crashed agent or one that exits because it cannot rebind, the
restart does not rerun generation activation. katlc-agent is ordered after
`katl-generation-activate.service` without requiring it: that oneshot stays
inactive after it runs so that each `systemd-sysext.service` or
`systemd-confext.service` start runs it again, and a start job queued by the
agent would run it too. If such a rerun failed, the agent could not start and
the revert meant to restore access would meet the same failure. Boot health
still requires activation through the extension units, so a failed
activation fails boot health while the agent stays reachable to report it.
`TestAgentRerunsNoUnit` and `TestAgentFollowsGenerationActivation` check this
against the runtime unit graph.

## Evidence for a trust change

`NodeStatus.management.served_client_trust` describes the listener that
answered the request. The TLS config builder fingerprints the DER of each
certificate it adds to the client CA pool. The listener carries those
fingerprints until it stops serving, even if installed state on disk changes
while calls drain. A plaintext listener reports a present, empty set,
including when the agent falls back after a trial fails to load.

A trial listener also reports its trial ID and target authority. The existing
transition record gives the durable trial state and deadline, and the existing
authentication and authority fields describe the installed mode. Commit
publishes the installed mode and removes the trial record. The listener then
clears its trial fields while keeping the same CA pool.

Rotation, recovery, and enable-mTLS require a timely status reply from the
bound node over the credential they are verifying. An mTLS reply must report
the exact expected set. Rotation and recovery require committed mTLS with
only the replacement CA. A replacement-only report proves removal of the old
CA directly. Enable-mTLS also checks that the answering listener and the live
trial record agree before it confirms within the trial deadline. It checks
committed trust and health before advancing to the next node. A final saved
context refresh verifies committed trust and the bound installation before
publishing workstation state. Confirmation carries the verified trial ID,
which the agent checks under the identity lock before committing. A lost
confirmation reply can be resolved by a fresh committed status through the
replacement credentials.

If recovery observes the replacement installed over SSH and cannot reach the
listener over mTLS, it reruns the pinned SSH rotation command with the installed
authority as the expected replacement. That request can only reload the
already installed credentials. Recovery still checks the listener after reload
before publishing the new secrets reference.

A node can use compatibility behavior when its initial timely status omits
the report. Once it provides a report, later omissions stop verification.
Older clients can still confirm a trial using only its authority. Recovery and an already configured rotation retain
their positive reachability and identity checks. Legacy rotation requires
positive replacement status before and after a timely issuer-specific TLS
`unknown_ca` from the authenticated old-credential probe connection.
Enable-mTLS retains the positive trial, installed-mode, identity and health
checks available from older agents. Its initial plaintext discovery guard
still requires a timely HTTP/2 server preface before retrying a failed mTLS
preflight over plaintext.

A mismatched report, expired request, generic TLS alert, EOF, result from
another handshake, or unscoped gRPC `Unauthenticated` cannot establish
trust. If verification remains inconclusive, the command stops and tells the
operator how to resume with the same secrets.

## Accepted risk: replacement key delivery

Begin carries the replacement server private key over the trusted-network
connection it is moving away from. This is an accepted risk rather than a gap:
a node in trusted-network mode already grants full management to anyone who
can reach port 9443, so sending the key over that channel exposes nothing
that the mode does not already expose. Generating the key on
the node and returning a CSR is tracked in #671 and is not part of this
release.
