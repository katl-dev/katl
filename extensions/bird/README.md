# BIRD system extension

This definition builds the generic BIRD software and `bird.service` published
at `ghcr.io/katl-dev/katl/extensions/bird`. It deliberately contains no API
VIP policy and no `/etc/bird.conf`.

Operators select the OCI bundle through `systemExtensions`, supply their native
BIRD configuration through `configuration.files`, and may change the unit
with normal `units[].dropIns`. Katl treats the application as opaque.
