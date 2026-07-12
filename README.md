# Katl

Katl produces and maintains KatlOS: an experimental, installable,
upgradeable, systemd-native Kubernetes node OS.

KatlOS treats Kubernetes clusters as a first-class workload. Users customize it
by supplying Katl YAML or configuration, which `katlc` validates and compiles
into sysext/confext generations. Those generations are activated with
rollback-aware runtime state while staying close to native systemd, Linux, and
kubeadm artifacts.

Katl is early-stage software. Interfaces, workflows, generated artifacts, and
runtime behavior are expected to change. Do not use Katl for production
clusters.

For the current install boundary and PXE/USB handoff examples, see
[`docs/installing.md`](docs/installing.md). Before evaluating a release, read
the [KatlOS support boundary](docs/support.md) for the tested surface, trust and
compatibility limits, recovery scope, and required bug-report evidence.
