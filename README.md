# Katl

Katl is an experimental systemd-native Kubernetes node OS builder.

It is designed to produce a stripped-down Linux node OS that treats Kubernetes
clusters as a first-class workload. Katl focuses on modern systemd primitives,
immutable and versioned node runtime artifacts, kubeadm-ready bootstrapping, and
GitOps-focused node configuration that compiles into native systemd, sysext, and
confext artifacts.

Katl is early-stage software. Interfaces, workflows, generated artifacts, and
runtime behavior are expected to change. Do not use Katl for production
clusters.
