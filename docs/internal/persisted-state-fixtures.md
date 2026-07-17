# Persisted State Fixtures

Katl keeps static fixtures for the current persisted record shapes. They prove
that readers validate identity, paths, digests, enums, timestamps, and replay
behavior using representative data that is independent of the current writer.

These fixtures are not, by themselves, a backward-compatibility promise. All
current formats are `v1alpha1`; the support boundary permits incompatible
changes between alpha releases and may require reinstall. When a current shape
changes, update or replace its fixture and review the affected lifecycle
behavior.

Retain an older fixture only when a shipped Katl release explicitly promises to
read that version. At that point the fixture should name the supported release
or version and exercise the strongest public reader. Do not retain obsolete
development formats merely to keep a byte-level golden or an unshipped design
working.

Negative fixtures should cover durable validation behavior such as unsupported
versions, missing payloads, unsafe unknown fields, identity mismatches, digest
mismatches, invalid enum values, and malformed timestamps.
