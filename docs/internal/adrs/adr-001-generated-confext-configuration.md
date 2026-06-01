# ADR-001: Katl-native configuration becomes generated confext

Status: accepted.

Date: 2026-05-31.

This ADR records the accepted configuration model: Katl-native input is rendered
by Katl into generated confext. The rejected bootstrap option is documented only
so future work does not reopen it by accident.

## Context

Katl builds a Fedora-derived, systemd-native runtime OS for kubeadm-ready
Kubernetes nodes. The runtime root is immutable and versioned. Durable node state
lives under writable state partitions, primarily `/var`.

Katl needs a way to configure `/etc` while preserving:

```text
typed installer validation
native systemd/Linux file semantics
generation-scoped rollback
local QEMU install tests
later runtime configuration updates
```

Users should not be required to build or provide confext artifacts directly.
They should provide Katl configuration in known domains. Katl then validates
that input and renders the generated configuration artifacts.

## Decision

Katl uses a Katl-native install manifest for initial node input.

For first install, `katlos-install`:

```text
reads the install manifest
validates user-supplied configuration
rejects configuration outside known domains
rejects attempts to write arbitrary /etc paths or Katl-owned paths
renders a generation-scoped generated confext tree or image
writes extension-release metadata
stages the generated confext under /var/lib/katl/generations/<generation-id>/
records it in generation metadata with the selected root slot and sysext set
```

For later runtime configuration changes, a Katl runtime agent can perform the
same logical operation on an installed node:

```text
receive desired Katl configuration
validate trust and policy
render a new generated confext generation
stage it under /var/lib/katl/generations/<generation-id>/
activate it with the selected generation
record success, failure, and rollback state
```

The runtime agent is later work. The install-time generated confext layout must
leave room for that model.

## User-Facing Input

Users provide Katl configuration and an install manifest. Configuration is
domain-scoped. A domain can preserve native syntax while still keeping Katl in
control of the destination path and apply behavior.

Example:

```text
networkd domain
  accepts native .network, .netdev, and .link content
  renders into /etc/systemd/network/
  applies through systemd-networkd/networkctl behavior
```

This is a thin abstraction, not an arbitrary `/etc` passthrough.

Users do not provide:

```text
prebuilt confext artifacts in the default path
arbitrary `/etc` file paths
host account definitions
sudo, PAM, passwd, shadow, or sysusers policy
root disk partitioning or root filesystem policy
kubeadm output under /etc/kubernetes
```

`/etc/kubernetes` is kubeadm/kubelet mutable state. It is projected from the
writable state partition and must not be owned by generated confext.

## Host Account Boundary

Katl owns host user and SSH policy.

The runtime host users are:

```text
root
  password locked; no SSH login

katl
  the only SSH login account; key-only authentication

package/system users
  created by the base packages that require them

katl-agent
  optional later no-login service user
```

User-supplied generated confext input must not write account or authentication
control files:

```text
/etc/passwd
/etc/shadow
/etc/group
/etc/gshadow
/etc/sudoers
/etc/sudoers.d/*
/etc/pam.d/*
/etc/security/*
/etc/subuid
/etc/subgid
/etc/sysusers.d/*
/etc/ssh/sshd_config
/etc/ssh/sshd_config.d/*
```

Users supply SSH public keys through Katl config. Katl renders authorized keys
for the `katl` account and renders sshd policy.

## Generated Confext Storage

Generated confext content is generation-scoped:

```text
/var/lib/katl/generations/<generation-id>/confext/
  etc/
    extension-release.d/
      extension-release.katl-node
    ...
```

Generated confext must be selected with the same generation metadata as the root
slot, UKI, kernel command line, and sysext set. It must not drift independently
through a single global mutable `current` tree.

The generated confext can initially be a directory tree. Katl may package it as
a raw confext image later if that adds value.

Generated confext does not require a separate signature in the default path
because it is generated on the target node from already trusted Katl input.
Signing generated confext artifacts can be reconsidered later for distribution
or hardening, but it is not part of the default first implementation.

## Rejected Option

Katl does not use Ignition for installer or runtime configuration.

It was rejected because it would introduce another configuration language and a
separate first-boot phase between `katlos-install` and the runtime configuration
agent. Katl already needs to validate install input, own destructive disk
actions, verify artifacts, generate confext, coordinate generation metadata, and
support later runtime-generated configuration generations. Keeping those
responsibilities inside Katl gives one source of truth and avoids a
three-phase installer/bootstrap/runtime model.

## Consequences

`katlos-install` must implement configuration validation and generated confext
materialization itself.

Generated confext validation is security-sensitive because it writes effective
runtime `/etc` content. Validation must reject:

```text
unknown configuration domains
raw user-supplied /etc paths outside a domain renderer
path traversal inside any domain renderer
duplicate normalized outputs
unsafe file modes or owners
unsupported file types
attempts to own /etc/kubernetes
attempts to own host account or SSH policy files
writes outside the generated confext root
```

The runtime boot path must activate only the selected generation's confext.
Rollback must switch root, sysext, and confext together.

## Follow-Up Work

Implementation follow-up:

```text
replace arbitrary etc.files handling with known configuration domain renderers
enforce the fixed host user and SSH policy in generated confext validation
render Katl-owned sshd policy and katl authorized keys
wire generated confext activation into generation selection
prove the boot ordering in QEMU
define the later runtime agent input and trust policy
```
