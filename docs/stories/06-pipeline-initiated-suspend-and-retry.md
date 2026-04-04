# Story 6: Pipeline can suspend itself and request a timed retry

**As a** platform engineer,
**I want** a configure pipeline to be able to suspend itself and request that Kratix retry it after a given time,
**So that** pipelines can safely wait on external conditions (e.g. a dependency that isn't ready) without failing permanently or busy-looping.

**Background:**
This is a pipeline-initiated mechanism. The pipeline itself — not the platform engineer — signals to Kratix that it needs to pause. It does this via a workflow control file written during execution. Kratix reads this signal, halts the workflow, and resumes automatically at the specified time.

This is distinct from Story 4 (platform engineer manually triggers a re-run) and Story 5 (spec change triggers a re-run). Here the pipeline is in control of its own retry schedule.

## Acceptance Criteria

- [ ] A pipeline can signal Kratix to suspend the workflow by writing a workflow control file with a retry-at time
- [ ] When Kratix detects the suspend signal, no further pipelines in the sequence are started
- [ ] A warning event is emitted on the Promise/resource request indicating the workflow is suspended
- [ ] The Promise/resource request status reflects that the workflow is suspended
- [ ] The checkpoint (which pipeline was active) is recorded in the Promise/resource request status
- [ ] When the retry time is reached, Kratix automatically resumes the workflow from the suspended pipeline — not from the beginning
- [ ] A platform engineer can also force an immediate resume by manually removing the suspend label, which resumes from the checkpoint
- [ ] Pipelines that had already completed before the suspend are not re-run on resume

## Out of Scope

- What happens if the spec changes while the workflow is suspended (covered in Story 7)
- What happens if the platform engineer triggers a manual reconcile while suspended (covered in Story 7)

## Engineering Notes

- The suspend signal is applied to the parent object as a label (`kratix.io/workflow-suspended: "true"`) by the work-creator after reading the pipeline's control file
- The retry time is stored in the parent object's status (`nextRetryAt`) and checked on each reconcile
- The checkpoint pipeline index is stored in the parent object's status (`suspendedPipelineIndex`)
- Resume from checkpoint is detected when the suspend label is absent but the checkpoint index is present in status
