# BIRD system extension

This definition builds the generic BIRD software and `bird.service` published
at `ghcr.io/katl-dev/katl/extensions/bird`. It deliberately contains no API
VIP policy and no `/etc/bird.conf`.

Operators select the OCI bundle through `systemExtensions`, supply their native
BIRD configuration through `configuration.files`, and may change the unit
with normal `units[].dropIns`. Katl treats the application as opaque.

The mkosi recipe uses the runtime root as its base tree. Changes to the shared
runtime profile or builder require advancing `KATL_EXTENSION_ARTIFACT_VERSION`
in `extension.env`, even when the upstream BIRD version stays the same. Keep
`KATL_EXTENSION_PAYLOAD_VERSION` tied to that upstream version. The pinned builder digest in
`mkosi-builder.lock` is not a recipe input: moving the pin rebuilds and checks
the extension but does not require a new artifact version.
