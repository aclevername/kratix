# Story 7: Restart workflow from the beginning when state changes during suspension

**As a** platform engineer,
**I want** Kratix to restart the configure workflow from the beginning — rather than resuming from the suspension checkpoint — if something meaningful changed while the workflow was suspended,
**So that** a resumed pipeline never operates on a stale view of the world.

**Background:**
Builds on Story 6 (pipeline-initiated suspension). There are two situations where resuming from a checkpoint is unsafe and a full restart is correct:

1. The Promise or resource request spec changed while the workflow was suspended
2. The platform engineer explicitly triggered a manual re-run (Story 4) while the workflow was suspended

In both cases, resuming mid-sequence could mean later pipelines run against a different spec than earlier ones did. The safe behaviour is to restart from pipeline 0.

## Acceptance Criteria

- [ ] If the spec changes while a workflow is suspended, Kratix restarts the workflow from pipeline 0 when it next reconciles — not from the suspension checkpoint
- [ ] If the platform engineer applies the manual reconciliation label (Story 4) while a workflow is suspended, Kratix restarts from pipeline 0 — not from the checkpoint
- [ ] In both cases, the suspension label is cleared and all pipeline status counters are reset before the restart
- [ ] The restarted workflow produces the same events and status updates as an initial run (Story 1 / Story 2)

## Out of Scope

- Normal resume from checkpoint when nothing has changed (covered in Story 6)

## Engineering Notes

- This is handled before `ReconcileConfigure` is called: the controllers detect the condition and set an internal restart label (`internal.workflows.kratix.io/run-from-start`) which `ReconcileConfigure` reads to run from pipeline 0
- The internal restart label is not intended to be set by users directly
- Spec change detection for resource requests uses a `suspendedGeneration` field in status compared against the current generation
