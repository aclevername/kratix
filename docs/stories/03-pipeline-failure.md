# Story 3: Surface pipeline failure to the platform engineer

**As a** platform engineer,
**I want** to be clearly informed when a configure pipeline fails,
**So that** I can diagnose the problem and take action.

**Background:**
A configure pipeline Job fails (i.e. the Kubernetes Job reaches a failed terminal state). The workflow must stop, surface the failure, and wait for human intervention.

## Acceptance Criteria

- [ ] When a configure pipeline Job fails, the Promise/resource request status reflects that the configure workflow has failed
- [ ] A warning event is emitted on the Promise/resource request: `"A <promise|resource>/configure Pipeline has failed: <pipeline-name>"`
- [ ] No further pipelines in the sequence are started after a failure
- [ ] The workflow does not automatically retry — it waits for the platform engineer to act
- [ ] The failure state is stable: repeated reconciles do not change state or emit duplicate events while the failure stands

## Out of Scope

- How the platform engineer recovers / retries (covered in Story 4)

## Engineering Notes

- A Job is considered failed when its Kubernetes status conditions include `JobFailed` or `JobSuspended`
- Status updates: `workflowsFailed` counter is set to 1; pipeline phase is marked as `Failed`; the `Reconciled` condition on the parent object is set to `Failing`
