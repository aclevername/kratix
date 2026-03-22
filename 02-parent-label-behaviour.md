# Prompt

Lets go deeper into what the different labels might be on the parent object, and hwo the changes the reconciler behaviour. For example, the manual reconciliation label. Proviode a to the point summary, dont bloat the response.

# Findings

`lib/workflow/reconciler.go` itself really only cares about 3 behavior-changing parent labels here.

## `kratix.io/manual-reconciliation`

- Strongest control flag.
- In configure reconciliation, it forces a fresh run from the start:
  - the reconciler stops trusting the latest Job as the current pipeline
  - resets workflow progress/failure state
  - suspends any running Job
  - removes the label
  - starts again at pipeline 0
- In delete reconciliation, it suspends any running delete Job, removes the label, and recreates the delete pipeline.

## `internal.workflows.kratix.io/run-from-start`

- Configure-only restart flag.
- Forces `pipelineIndex=0` and `completedCount=0`.
- The reconciler removes the label before creating the next Job.
- It also drives workflow status back toward pending/running.
- The delete path does not use it.

## `kratix.io/workflow-suspended`

- Mostly a controller-level gate rather than a direct workflow-engine trigger.
- While it is `true`, the Promise/resource controllers typically skip workflow reconciliation.
- After it is removed, the reconciler checks status for a suspended pipeline and resumes from that pipeline instead of starting over.
- In practice, the controllers often pair removal of this label with `run-from-start=true` when they want a forced rerun instead of a true resume.
