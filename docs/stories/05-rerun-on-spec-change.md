# Story 5: Re-run the configure workflow when the Promise or resource spec changes

**As a** platform engineer,
**I want** the configure workflow to re-run automatically when I update a Promise or a resource request's spec,
**So that** the outputs always reflect the current desired state without me having to manually trigger anything.

**Background:**
The platform engineer updates a Promise (e.g. changes pipeline image or configuration) or a user submits an updated resource request. Any in-flight pipeline Job for the previous spec is no longer relevant and should not be allowed to complete and overwrite outputs with stale data.

## Acceptance Criteria

- [ ] When a Promise or resource request spec changes, the configure workflow re-runs from the first pipeline
- [ ] If a pipeline Job from the previous spec is currently running, it is suspended and then deleted before the new workflow begins
- [ ] The new workflow run uses the updated spec — not the previous one
- [ ] Success/failure counters on the status are reset when the new run begins
- [ ] The re-run produces the same pipeline events and status updates as an initial run (Story 1 / Story 2)

## Engineering Notes

- Spec change detection is hash-based: Job labels carry a hash of the pipeline/resource spec. A mismatch between the running Job's hash and the current spec's hash is what triggers this path
- The old Job is suspended (not deleted) to preserve logs for debugging
- This is distinct from Story 4 (manual re-run): the trigger here is an observed spec change, not an explicit user label
