# Story 2: Run multiple configure pipelines in sequence

**As a** platform engineer,
**I want** to define multiple configure pipelines on a Promise that run one after another,
**So that** I can compose complex workflows from discrete, ordered steps without managing the sequencing myself.

**Background:**
Builds on Story 1. A Promise defines two or more configure pipelines. Each must complete successfully before the next begins.

## Acceptance Criteria

- [ ] When a Promise defines multiple configure pipelines, they are run in the order they are defined
- [ ] The next pipeline does not start until the current pipeline's Job has completed successfully
- [ ] The Promise/resource request status reflects how many pipelines have completed (e.g. `workflowsSucceeded: 2`)
- [ ] Each pipeline start emits its own `"Configure Pipeline started: <pipeline-name>"` event
- [ ] If a pipeline is still running, subsequent reconciles do not start the next pipeline or create duplicate Jobs
- [ ] When all pipelines have completed, the overall configure workflow status reflects success

## Out of Scope

- Failure handling (covered in Story 3)
- Manual re-run or restart

## Engineering Notes

- Pipeline identity is determined by matching Job labels (pipeline name + resource/spec hash) — the sequencing logic depends on this, not on Job name or creation order alone
- Status counter `workflowsSucceeded` increments after each pipeline completes, not only at the end
