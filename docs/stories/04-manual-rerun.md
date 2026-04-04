# Story 4: Manually trigger a configure workflow re-run

**As a** platform engineer,
**I want** to trigger a full re-run of the configure workflow on demand,
**So that** I can recover from a failure or force re-execution without deleting and recreating the Promise or resource request.

**Background:**
The platform engineer signals intent to re-run by applying a well-known label to the Promise or resource request. Kratix detects this and restarts the configure workflow from the first pipeline, regardless of current state.

## Acceptance Criteria

- [ ] When the platform engineer applies the manual reconciliation label to a Promise or resource request, the configure workflow re-runs from the first pipeline
- [ ] If a pipeline Job is currently running when the label is applied, it is suspended and deleted before the re-run begins
- [ ] The label is removed from the object automatically after the re-run is triggered, so it does not re-trigger on the next reconcile
- [ ] Success/failure counters on the status are reset to zero when the re-run begins
- [ ] The re-run produces the same pipeline events and status updates as an initial run (Story 1 / Story 2)

## Engineering Notes

- The trigger mechanism is a label key on the parent object; the value must be `"true"`
- Label removal must happen before the new Job is created to prevent a loop
- `workflowsSucceeded` and `workflowsFailed` are both reset to `0` on re-run
