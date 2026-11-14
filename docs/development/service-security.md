# Base service security

KatlOS restricts each base service according to its host responsibilities.
The management API and operator SSH access remain administrative interfaces:
use the trusted management network or opt-in management mTLS described in
[management access](../operations/access.md).

## Policy ownership

Security drop-ins live under `/usr/lib/systemd/system`. They apply even when
the selected generation supplies a complete unit under `/etc/systemd/system`.
Keep the shared baseline attached to explicit Katl-owned units; do not apply
it globally to user services or container workloads.

The common Katl policy prevents privilege acquisition, writable executable
memory, execution-domain changes, realtime scheduling, and nonnative system
calls. Service-specific policies additionally restrict capabilities, devices,
filesystems, namespaces, and socket families. These controls also apply to
commands a service executes directly. Commands started through PID 1 run under
the destination unit's policy.

Do not treat `systemd-analyze security` scores as a pass/fail threshold. A
service that manages mounts, loads modules, or launches containers needs
privileges an observer does not. Review the resulting Linux process and
filesystem restrictions as well as the unit configuration.

## Katl services

| Service | Responsibility and restrictions |
| --- | --- |
| `katl-api-proxy` | Uses a dynamic unprivileged account and no capabilities. Reads the proxy configuration and public Kubernetes CA; writes only its runtime status. Restricts devices, kernel interfaces, namespaces, executable memory, system calls, and socket families. |
| `katl-console` | Reads node state, journals, the container runtime, and Kubernetes status. Writes its runtime snapshot and accesses only the dashboard and log terminals. Retains `CAP_SYS_ADMIN` for the Linux console log-redirection ioctl; its syscall filter denies mounts. Runtime files survive console restarts because the installer publishes its handoff independently. |
| `katlc-agent` | Coordinates privileged installation state, host configuration, networking, volumes, extensions, upgrades, and Kubernetes operations. Retains root, host devices, module access, and the host mount namespace. Denies direct clock changes, reboot, raw port I/O, debugging, swapping, obsolete calls, and executable writable memory. Reboots go through systemd. |
| `katl-generation-activate` | Verifies the selected generation, publishes extension links, arms boot recovery, and sets the host hostname. Retains `CAP_SYS_ADMIN` for hostname activation; restricts writable paths and denies mount operations. |
| `katl-host-config-prepare` | Loads selected modules, applies sysfs settings, and notifies systemd and udev. Retains the capabilities needed for those host operations. |
| `katl-host-config-verify` | Reads native settings and module-provider files, and records verification in the selected generation. Has no capabilities and a read-only filesystem outside generation records. Module files must remain readable for provider verification. |
| `katl-boot-health`, `katl-boot-recovery`, `katl-boot-deadman` | Record boot outcomes and select validated boot entries. Retain access to the ESP, EFI variables, and boot/generation records, including the capability to clear EFI variables' immutable flags; restrict other writes, socket families, and system calls. |
| `katl-ssh-host-keys` | Creates or preserves persistent host keys. Has no capabilities, private devices and networking, a restrictive umask, and write access only to the host-key directory. |
| `katl-runtime-handoff-status` | Reads installation state and records runtime handoff. Has no capabilities, private devices and networking, and write access only to installation records. |
| `katl-system-extensions-reload`, `katl-system-extensions-activate` | Reload or activate units through PID 1 and record activation outcomes. Restrict direct filesystem and kernel access; activation can update generation records. |
| `katl-kubeadm-activate`, `katl-volumes-activate`, `katl-endpoint-activate` | Start the corresponding targets or endpoint service through PID 1. Have no capabilities or direct writable host paths. |
| `katl-runtime-boot-signal`, `katl-state-projection-check` | Emit progress markers using unprivileged dynamic accounts, without capabilities, host writes, or network access. |
| Power-control transients | Execute fixed `systemctl reboot` or `systemctl poweroff` commands after the management request has returned. Retain root access to PID 1; request validation fixes the command, target, and scheduling delay. |
| Host-upgrade preparation transient | Uses the existing capability-free, private-device/network preparation sandbox. Direct writes are limited to its temporary work directory; the ESP and EFI variables are inaccessible. |

## Distribution and Kubernetes services

| Service family | Security boundary |
| --- | --- |
| `systemd-networkd`, `systemd-resolved`, `systemd-logind`, `systemd-journald`, `systemd-userdbd`, `systemd-nsresourced` | Retain upstream service-specific sandboxes and capability sets. Networking needs its host network interfaces; logging, login, user lookup, and namespace allocation have distinct privileged responsibilities. |
| `systemd-udevd`, module loading, sysctl, tmpfiles, filesystems, sysext/confext, boot/TPM, and device helpers | Retain native host access needed to initialize hardware, apply configuration, merge extensions, and manage boot state. Filesystem namespaces must not hide mounts from their host consumers. |
| `dbus-broker` | Runs its broker and launcher as the distribution's bus account after initialization. Additional restrictions cover kernel interfaces, namespaces, executable memory, socket families, and system calls. Activation remains delegated to systemd. |
| `gssproxy`, `rpc-gssd`, `rpc-statd-notify` | Retain NFS credential caches, key interfaces, and recovery state. Add process restrictions without hiding credential paths or preventing the vendor's NFS `/proc` interaction. GSS proxy retains the vendor's privilege-transition policy. |
| `dm-event`, `lvm2-lvmpolld`, LVM monitoring | Retain host devices, mount propagation, and scheduling needed for volume monitoring and recovery. Add process restrictions without imposing private device or mount namespaces. |
| `sshd` and login/rescue sessions | Authenticate operator public keys. Password, keyboard-interactive, and GSSAPI authentication are disabled. Sessions retain the operator's administrative environment; a daemon sandbox must not silently restrict supported root administration. |
| `containerd`, shims, `crun`, and `kubelet` | Retain host namespaces, devices, cgroups, mounts, and workload privilege semantics. Do not inherit a global `NoNewPrivileges`, capability ceiling, syscall filter, or private filesystem namespace into containers. Kubernetes and the operator's workload configuration own pod restrictions. Kubelet authentication and authorization remain configured through the supported Kubernetes workflow. |

Only normal DNS resolution is enabled by default: resolved's LLMNR and mDNS
responders are disabled. Katl does not generate additional SSH listeners on
Unix or vsock sockets. The configured TCP SSH daemon owns operator SSH access.

The base image masks unused home-directory, image-import, portable-service,
and sysupdate D-Bus daemons. It also masks sysupdate's separate socket API,
automatic update/reboot timers, and post-update notification services. Katl's
upgrade path uses the sysupdate CLI and owns activation and reboot ordering.
The CNI DHCP daemon is excluded alongside its unsupported plugin.

Base executables do not grant privileges through setuid/setgid bits. Rootless
UID/GID mapping helpers are excluded. Supported host administration starts
through root SSH or the management agent; root can still run filesystem and
account tools without setuid helpers. This does not disable user namespaces
used by supported container workloads.

Host core dumps are neither stored nor processed into backtraces by default,
because process memory can contain management credentials, cluster keys, and
workload data. Crash metadata remains in the journal. The coredump default
also applies to container crashes handled by the host.

Optional BIRD, endpoint-advertiser, and GPU extensions retain their own unit
policies and capability requirements. They are not default-running base
services. User-installed CNIs, storage agents, and other Kubernetes applications
remain operator-owned.

## Validate a policy change

Use `systemd-analyze verify` against the built image and inspect the effective
units on an installed node. Exercise the affected boot, configuration,
Kubernetes, upgrade, and reboot paths through `katlctl` in the persistent
development VM.

Run the service sandbox regression through the supported VM harness:

```sh
scripts/vmtest-run --artifact-set=install ./internal/vmtest \
  -run '^TestInstalledRuntimeServiceSecuritySmoke$' -count=1
```

Its probes execute under the console and proxy service policies, check their
necessary runtime writes, and attempt forbidden writes, credential reads,
executable memory allocation, namespace creation, mounts, and packet sockets.
It also checks that host orchestration keeps the host mount namespace and
that packaged executables cannot elevate privileges through setuid/setgid.

Consult [systemd.exec](https://www.freedesktop.org/software/systemd/man/latest/systemd.exec.html)
for inherited restrictions and mount propagation, and
[coredump.conf](https://www.freedesktop.org/software/systemd/man/latest/coredump.conf.html)
for core-processing behavior.
