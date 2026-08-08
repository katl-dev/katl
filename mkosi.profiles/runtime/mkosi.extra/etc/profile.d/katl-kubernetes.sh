# Use kubeadm's root-only administrative context in interactive root shells.
# Keep an explicit operator selection intact, including an intentionally empty
# KUBECONFIG used while diagnosing client behavior.
if [ -z "${KUBECONFIG+x}" ] && [ "${EUID:-$(id -u)}" -eq 0 ] && [ -r /etc/kubernetes/admin.conf ]; then
    export KUBECONFIG=/etc/kubernetes/admin.conf
fi
