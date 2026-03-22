# Refactor Prompt

You are refactoring the configure workflow path in `lib/workflow/reconciler.go`.

Before making changes, read these repo notes first. They will be provided alongside this prompt and should be treated as required context:

- `01-caller-of-reconcilers.md`
- `02-parent-label-behaviour.md`
- `03-configure-job-state-worlds.md`

## Scope

- Refactor only the configure path in `lib/workflow/reconciler.go`.
- Do not refactor `ReconcileDelete` in this change.
- Do not change the public callers for now.
- Preserve the existing external entrypoint shape: `ReconcileConfigure(opts Opts)`.

## Intent

The current configure reconciler is hard to read because it mixes these concerns together:

- observing the world from Jobs and parent labels/status
- deciding what pipeline should run next
- performing side effects
- synchronising status

The current code also leans too heavily on "look at the newest Job and infer the world from that". That is difficult to reason about.

The refactor should move toward this mental model instead:

- observe the state of each pipeline in order
- for each pipeline, determine whether it has an appropriate and up-to-date Job
- if a pipeline is already successfully complete, continue to the next one
- the first pipeline that is not complete is the one that matters
- based on its observed Job state, either wait, create, fail, suspend, or clean up

## Design constraints

- Keep the code readable and explicit.
- Prefer WET over DRY if it makes the flow easier to understand.
- Avoid clever abstractions.
- Keep plan and execute together in the control-flow code. Do not split them into separate planner/executor layers.
- Status code should be separated from action-taking code. Treat status as "observed state projection", not as something intertwined with action selection.
- It is acceptable if the refactor makes additional Kubernetes API calls, if that makes the code easier to understand.
- Add clear logging around major branches and decisions.
- If the refactor exposes behavior that is obviously buggy or ambiguous, prefer the version that produces the best code and the clearest behavior. Do not preserve confusing behavior just because it exists today.

## Desired structure

Keep `ReconcileConfigure(opts Opts)` as the top-level entrypoint, but reorganise internals so the flow is obvious.

The target shape should be roughly:

1. Observe configure world.
2. Sync configure status from the observed world.
3. Take action for the first incomplete pipeline.

You do not need to follow these exact names, but the structure should be close to this:

- `ObserveConfigureWorld(...)`
  - gathers all information needed for decision-making
  - should primarily think in terms of pipelines-in-order, not "latest job overall"
  - should classify each pipeline into an explicit observed state such as:
    - no current Job
    - current Job running
    - current Job succeeded
    - current Job failed
    - current Job suspended
  - should also capture control inputs from parent labels/status:
    - manual reconciliation
    - run-from-start
    - resume-from-suspended
    - suspended pipeline index
  - should still detect anomalous situations that matter globally, for example:
    - a later pipeline has a running Job before an earlier pipeline is complete
    - a Job exists but is for an outdated spec

- `SyncConfigureStatus(...)`
  - all status/counter/condition updates should live here
  - derive status from the observed world
  - keep this separate from action-taking logic
  - this may still use existing `resourceutil` helpers where helpful, but the resulting flow should be easier to read than the current interleaving

- configure action flow
  - keep plan and execute together
  - once the observed world is available, the main control flow should read clearly top-to-bottom
  - avoid hidden coupling between status code and action code

## Specific guidance

- Do not make the control flow depend primarily on the single newest Job across all pipelines.
- Prefer looking at each pipeline's newest relevant Job.
- Be explicit about what counts as the "current" Job for a pipeline.
- Be explicit about what counts as "up-to-date".
- If hashes or labels are required to decide whether a Job belongs to the current pipeline/spec, keep that logic explicit and easy to find.
- If an anomalous running Job for a later or unrelated pipeline must be suspended, make that branch obvious and well logged.
- Make manual reconciliation behavior easy to follow.
- Make run-from-start behavior easy to follow.
- Make resume-from-suspended behavior easy to follow.
- Make it obvious when the reconciler is:
  - waiting
  - creating a pipeline
  - failing the workflow
  - suspending a Job
  - cleaning up

## Callers

- Do not change the callers at this stage.
- `Opts` can remain as-is externally.
- Internal helper signatures may change freely if that improves readability.

## Tests

Tests are required.

- Use Ginkgo/Gomega.
- Prefer readability over minimising repetition.
- Add or update tests so the configure path is well covered after the refactor.
- Include characterization coverage for important existing behaviors that must remain true.
- Include focused tests for the new observed-world/status split where useful.

At minimum, cover these cases:

- no Jobs exist and pipeline 0 should start
- current pipeline Job is running
- current pipeline Job succeeded and reconciliation advances
- current pipeline Job failed
- current pipeline Job suspended
- manual reconciliation with a running Job
- run-from-start behavior
- resume-from-suspended behavior
- later/unrelated running Job is detected and suspended
- outdated/stale Job does not incorrectly count as the current pipeline's valid Job
- status projection remains correct for success, failure, running, reset, and resume cases

## Non-goals

- Do not refactor delete reconciliation in this change.
- Do not rewrite the callers.
- Do not optimise for minimal line count.
- Do not introduce abstraction layers whose main purpose is reuse rather than clarity.

## Deliverable

Produce a refactor that makes the configure reconciler significantly easier to read and reason about.

The final code should make it straightforward for a reader to answer:

- what world was observed?
- which pipeline is considered current?
- why did the reconciler wait, create, fail, suspend, or clean up?
- what status was written, and why?
