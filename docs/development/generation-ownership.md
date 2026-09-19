# Generation ownership

`internal/generation` owns the durable description and lifecycle of a node's
runtime: publication, activation, boot selection, health and promotion. The
offline installer, boot services and katlc use this implementation directly.
Installation does not require an agent running against the target filesystem.

The installer owns disk preparation and placement of runtime artifacts, node
identity and desired cluster inputs. After those inputs and the loader entry
are ready, it calls `generation.Initialize`. Initialization publishes the
immutable specification and initial status, then publishes boot selection last.
Repeating initialization with the same specification preserves existing health
and selection; a conflicting specification is rejected.

Initial selection names generation 0 as the default and pending boot target.
It contains no claim about which generation has booted or is active. Boot
health validates the actual kernel command line and root partition before
recording generation 0 as booted and healthy. The host-ready handoff requires
networking, operator access and management; Kubernetes bootstrap remains an
explicit subsequent operation.

Kubernetes operations own kubeadm mutation and its execution history. A
validated generation carries its immutable upgrade selection and durable health
record bound to the committing operation. Boot activation validates that record;
it does not reopen the operation journal. Retaining an operation receipt is not
a prerequisite for booting a healthy generation. Operation status also derives
completed boot health from the generation, so a stale pending flag in a receipt
cannot request another boot after a healthy generation has been superseded.

Node identity lives in `internal/nodeidentity` and writable node state, outside
generation selection. Generic kernel command-line and persisted-record helpers
also live outside the installer. The shared generation package does not depend
on installer or agent packages.

The on-disk spec/status envelopes and boot-selection schema remain unchanged.
A host fallback changes the selected runtime; it cannot undo Kubernetes or etcd
mutations, identity changes, or workload data.

Host upgrades use systemd-sysupdate as a transfer engine, not as the owner of
release ordering or boot retention. Under the node's upgrade lock, Katl finds
the active root by PARTUUID, labels it `katl_0` (protected), and prepares the
inactive root as `_empty`. The selected transfer always uses the local version
`1`; its completed root label is `katl_1`. These labels describe one transfer,
not a KatlOS release, and are prepared again from observed boot identity on the
next upgrade. Full release versions remain in generation metadata.

Each root slot has its own UKI match namespace. Before staging, Katl removes
only the inactive slot's previous UKI so sysupdate cannot mistake it for the
new candidate. Sysupdate cannot vacuum or overwrite the active slot's kernel,
and repeated upgrades reuse the two slot-owned files. Boot records retain the
concrete UKI paths, including paths from older installed releases.
Explicit older-release selection and repeated staging do not depend on the
lexical ordering or length of operator-visible release versions.
