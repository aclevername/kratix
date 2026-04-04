# Story 1: Run a configure pipeline when a Promise or Resource is installed

**As a** platform engineer, **I want** Kratix to automatically run the configure
pipeline when a Promise is installed or a resource request is submitted, **So
that** the pipeline's outputs are produced and scheduled to the appropriate
destinations without me having to trigger it manually.

**Background:** This is the foundational happy path. A single configure pipeline
is defined. No prior jobs exist. The pipeline runs to completion.

## Acceptance Criteria

- [ ] When a Promise with a configure pipeline is installed, a Kubernetes Job is
created for that pipeline
- [ ] When a resource request is submitted for a Promise with a configure
pipeline, a Kubernetes Job is created for that pipeline
- [ ] While the Job is running, subsequent reconciles do not create additional
Jobs
- [ ] When the Job completes successfully, the Promise/resource request status
reflects that the configure workflow succeeded
- [ ] A `"Configure Pipeline started: <pipeline-name>"` event is visible on the
Promise/resource request
- [ ] If no configure pipeline is defined, reconciliation is a no-op — no
errors, no events

## Out of Scope

- Multiple pipelines in sequence
- Failure handling
- Manual reconciliation or restart
- Suspension and resume
- Cleanup of old jobs

## Engineering Notes

- The Job and its supporting RBAC resources (ServiceAccount, Roles, Bindings)
are created together as a single unit of pipeline setup
- Status/condition updates are part of this story; they are the observable proof
the pipeline ran and succeeded
- Both Promise-level and resource-level pipelines share the same mechanism — the
distinction is which parent object owns the Job and receives the status update
