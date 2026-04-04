# Story 7: Force the configure workflow to restart from the beginning

**As a** platform engineer,
**I want** to force the entire configure workflow to restart from pipeline 0,
**So that** I can recover from a state where partial completion has left outputs in an inconsistent state and a clean re-run is needed.

**Background:**
Distinct from Story 4 (manual re-run triggered by label): this restart is triggered by the system in response to specific lifecycle events (e.g. a Promise being unpaused after a period of suspension). The platform engineer's action is indirect — they take a higher-level action and Kratix responds by restarting the workflow.

## Acceptance Criteria

- [ ] When the system determines a full restart is required, the configure workflow runs from pipeline 0 regardless of prior state
- [ ] Any in-progress pipeline Job is interrupted before the restart begins
- [ ] Success/failure counters are reset to zero
- [ ] The restart produces the same pipeline events and status updates as an initial run (Story 1 / Story 2)
- [ ] The restart trigger is cleared after it takes effect so the workflow does not loop

## Open Questions

- **User-facing trigger:** what is the platform engineer action that causes the system to issue a restart-from-start? Is this *only* an unpause event, or are there other triggers? This determines how to frame the acceptance criteria in user terms.
- **Overlap with Story 4:** if a restart-from-start and a manual re-run (Story 4) produce identical observable behaviour, should they be the same story with two trigger mechanisms, or kept separate?

## Engineering Notes

- The restart is signalled via an internal label (`WorkflowRunFromStartLabel`) set by the system, not directly by the user
- This label is removed after the restart is triggered, same pattern as the manual reconciliation label in Story 4
- The key difference from Story 4: the label is system-set, not user-set; the user's action is one level of indirection away
