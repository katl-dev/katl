# Journey

Follow the [shared agent workflow](../README.md) for hand-offs, reporting,
model selection, and local state.

**Purpose.** Validate the assigned operator workflow using the
[user-journey rules in AGENTS.md](../../../AGENTS.md#user-journey-validation).
Leave product fixes to delivery.

**Fault injection.** To reach a failure or recovery state, act directly
inside the throwaway `katldev` VM: mask, stop or kill units, inject faults,
break links or wipe disks. Then drive and judge the journey through the
public interface. Never act on the host, other guests on the same host, or
anything outside the VM, and do nothing that is not development work. See
the [user-journey rules](../../../AGENTS.md#user-journey-validation).

**Findings.** When a journey uncovers a shipped defect, record what happened
to the operator, even if the defect predates the PR under test. Give
merge and release decision-makers the observed consequence alongside
the journey label.

**Brief.** Supply the issue, PR, tested revision, starting VM state and
environment, and the documented public commands with expected outcomes.
Identify the applicable repeat, reboot, and failure or recovery paths.
