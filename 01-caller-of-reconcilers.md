# Prompt

the reconciler.go code is very complicated, and im considering refactoring it. but first i want to map out where its called, and what sort of inputs are provided. Explore, and report back your findings. keep the response short and to the point. Any clarifying questions?

# Response

No clarifying questions.

## Calls

- There’s one target file: [reconciler.go](/Users/jake/workspace/kratix/lib/workflow/reconciler.go).
- Its exported entrypoints are only reached from 4 production sites, via package vars in [promise_controller.go](/Users/jake/workspace/kratix/internal/controller/promise_controller.go#L64):
  - Resource configure: [dynamic_resource_request_controller.go](/Users/jake/workspace/kratix/internal/controller/dynamic_resource_request_controller.go#L234)
  - Resource delete: [dynamic_resource_request_controller.go](/Users/jake/workspace/kratix/internal/controller/dynamic_resource_request_controller.go#L687)
  - Promise configure: [promise_controller.go](/Users/jake/workspace/kratix/internal/controller/promise_controller.go#L985)
  - Promise delete: [promise_controller.go](/Users/jake/workspace/kratix/internal/controller/promise_controller.go#L1447)
- There’s also a test seam that swaps those funcs out: [export_test.go](/Users/jake/workspace/kratix/internal/controller/export_test.go#L10)

## Inputs

- Every caller builds the same `workflow.Opts` shape: parent `*unstructured.Unstructured`, `[]PipelineJobResources`, `workflowType` (`"promise"` or `"resource"`), namespace, jobs-to-keep, client/logger/recorder: [reconciler.go](/Users/jake/workspace/kratix/lib/workflow/reconciler.go#L27)
- `PipelineJobResources` come from the Promise spec via `GeneratePromisePipelines` / `GenerateResourcePipelines`: [promise_types.go](/Users/jake/workspace/kratix/api/v1alpha1/promise_types.go#L452), [pipeline_types.go](/Users/jake/workspace/kratix/api/v1alpha1/pipeline_types.go#L174)
- The reconciler relies heavily on generated job labels plus parent labels/status. It matches jobs by workflow type/action, promise/resource name, pipeline name, and hashes: [reconciler.go](/Users/jake/workspace/kratix/lib/workflow/reconciler.go#L401), [pipeline_factory.go](/Users/jake/workspace/kratix/api/v1alpha1/pipeline_factory.go#L421). It also reads parent labels/status for manual reconcile, run-from-start, suspended pipeline index, and workflow counters: [resourceutil/util.go](/Users/jake/workspace/kratix/lib/resourceutil/util.go#L20), [resourceutil/util.go](/Users/jake/workspace/kratix/lib/resourceutil/util.go#L404), [reconciler.go](/Users/jake/workspace/kratix/lib/workflow/reconciler.go#L185), [reconciler.go](/Users/jake/workspace/kratix/lib/workflow/reconciler.go#L259)

## Notable

- `ReconcileDelete` only uses the first delete pipeline and ignores the rest: [reconciler.go](/Users/jake/workspace/kratix/lib/workflow/reconciler.go#L72)
- I didn’t find any production caller setting `SkipConditions`; that path looks test-only.
