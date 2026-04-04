# Story 6: Pause and resume a configure workflow at a pipeline checkpoint

**As a** platform engineer,
**I want** to pause a running configure workflow and later resume it from where it stopped,
**So that** I can safely intervene mid-workflow without losing the progress of already-completed pipelines.

**Background:**
The platform engineer signals a pause by applying a well-known label to the Promise or resource request. Kratix records the checkpoint (which pipeline was in progress) and halts execution. When the label is removed, execution resumes from that pipeline — not from the beginning.

## Acceptance Criteria

- [ ] When the pause label is applied, no new pipeline Jobs are started
- [ ] If a pipeline Job is currently running when the label is applied, it is allowed to complete (or suspended — see open question)
- [ ] The checkpoint (which pipeline was active at pause time) is recorded in the Promise/resource request status
- [ ] When the pause label is removed, the workflow resumes from the recorded checkpoint pipeline — not from pipeline 0
- [ ] Pipelines that had already completed before the pause are not re-run
- [ ] Status and events during resume match those of a normal pipeline start (Story 1 / Story 2)

## Open Questions

- **Running job at pause time:** should an in-progress Job be suspended immediately when the pause label is applied, or allowed to run to completion before the pause takes effect?
- **Resume with no checkpoint:** if the workflow is paused before any pipeline has started, does resume start from pipeline 0?
- **Who removes the pause label?** Is this always a manual platform engineer action, or can it also be triggered by the system (e.g. after an automated gate passes)?
- **Visibility:** should pause/resume emit events on the parent object?

## Engineering Notes

- The pause state is tracked via a label on the parent object (`WorkflowSuspendedLabel`) and a status field recording the suspended pipeline index
- Resume is detected when the label is absent but the checkpoint index is present in status
- This is distinct from Story 5 (spec-change interruption): here the workflow is deliberately paused at a checkpoint and resumed from it, rather than being restarted from scratch
