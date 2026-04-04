# Story 8: Clean up old pipeline Jobs and stale outputs

**As a** platform engineer,
**I want** Kratix to automatically clean up old pipeline Jobs and any outputs from pipelines that no longer exist,
**So that** my cluster doesn't accumulate stale Kubernetes resources over time as Promises evolve.

**Background:**
Every configure workflow run creates at least one Job. Over time — especially with frequent updates or re-runs — these accumulate. Similarly, if a pipeline is removed from a Promise's definition, any outputs it previously produced (Work objects) should be removed.

## Acceptance Criteria

- [ ] After a configure workflow completes successfully, old Jobs for each pipeline are pruned so that no more than N historical Jobs are retained per pipeline
- [ ] The default retention limit is 5 Jobs per pipeline; this is configurable globally for the Kratix operator (not per Promise)
- [ ] The most recent N Jobs are always retained; older ones are deleted
- [ ] When a pipeline is removed from a Promise's definition, the Work objects it previously produced are deleted
- [ ] Cleanup does not affect currently running Jobs or Jobs within the retention window
- [ ] Cleanup happens automatically after workflow completion — the platform engineer does not need to trigger it

## Engineering Notes

- Retention is tracked per pipeline name — each named pipeline has its own independent Job history
- Work cleanup compares the current Promise spec's pipeline list against labels on existing Work objects; Works whose pipeline name no longer appears in the spec are deleted
- The global retention limit is set via `numberOfJobsToKeep` in the Kratix operator config (default: 5, minimum: 1)
- Stale RBAC resources (Roles/ClusterRoles from a previous pipeline definition) are cleaned up before each new pipeline run, not as part of end-of-workflow cleanup
