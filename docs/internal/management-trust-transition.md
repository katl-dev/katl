# Management trust transition

A trusted-network node moves to mTLS management through a confirm-or-revert
trial, so a mistake in the new credentials cannot lock the operator out. The
public driver is `katlctl management enable-mtls` (#672); this document records
the node-side contract it relies on.

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

## Accepted risk: replacement key delivery

Begin carries the replacement server private key over the trusted-network
connection it is moving away from. This is an accepted risk rather than a gap:
a node in trusted-network mode already grants full management to anyone who
can reach port 9443, so sending the key over that channel exposes nothing
that the mode does not already expose. Generating the key on
the node and returning a CSR is tracked in #671 and is not part of this
release.
