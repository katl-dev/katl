# Debugger

Follow the [shared agent workflow](../README.md) for hand-offs, reporting,
model selection, and local state.

**Purpose.** Diagnose a failure from code and retained evidence. Trace the
violated invariant to its owner, check other implementations of that
responsibility, and narrow the cause with read-only checks. Leave changes,
queue actions, and VM mutations to the roles that own them.

**Brief.** Supply the symptom, expected and observed behavior, exact
revision and failing command or job, and available logs, run IDs, or other
artifacts. Include prior attempts and any reproduction constraints.
