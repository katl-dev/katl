// Package nspawntest provides a small Go harness for host-dependent
// systemd-nspawn checks.
//
// The harness validates userspace and filesystem behavior in a prepared Katl or
// Fedora root. It is intentionally narrower than the QEMU vmtest harness:
// nspawn does not prove firmware, bootloader, kernel command line, disk layout,
// networking, kubeadm cluster joins, rollback, or VM agent behavior. Use it for
// deterministic checks of generated confext, sysext, systemd, and node-local
// filesystem artifacts before paying the cost of a full VM scenario.
package nspawntest
