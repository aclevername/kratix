package workflow

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/client-go/tools/record"

	"github.com/go-logr/logr"
	"github.com/syntasso/kratix/api/v1alpha1"
	"github.com/syntasso/kratix/internal/logging"
	"github.com/syntasso/kratix/lib/resourceutil"
	"gopkg.in/yaml.v2"
	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Opts struct {
	ctx          context.Context
	client       client.Client
	logger       logr.Logger
	parentObject *unstructured.Unstructured
	//TODO make this field private too? or everything public and no constructor func
	Resources          []v1alpha1.PipelineJobResources
	workflowType       string
	numberOfJobsToKeep int
	eventRecorder      record.EventRecorder
	namespace          string

	// Set by other controllers that use the Workflow engine
	SkipConditions bool
}

func (o *Opts) SetParentObject(parentObj *unstructured.Unstructured) {
	o.parentObject = parentObj
}

var minimumPeriodBetweenCreatingPipelineResources = 1100 * time.Millisecond
var ErrDeletePipelineFailed = fmt.Errorf("delete Pipeline Failed")

func NewOpts(ctx context.Context, client client.Client, eventRecorder record.EventRecorder, logger logr.Logger, parentObj *unstructured.Unstructured, resources []v1alpha1.PipelineJobResources, workflowType string, numberOfJobsToKeep int, namespace string) Opts {
	return Opts{
		ctx:                ctx,
		client:             client,
		logger:             logger,
		parentObject:       parentObj,
		workflowType:       workflowType,
		numberOfJobsToKeep: numberOfJobsToKeep,
		Resources:          resources,
		eventRecorder:      eventRecorder,
		namespace:          namespace,
	}
}

// ReconcileDelete deletes Workflows.
// The returned bool is passiveRequeue:
// true means reconcile should happen again, passively, when watched external
// resources are updated (for example a workflow Job changing state), rather
// than by issuing an explicit direct requeue from this function.
func ReconcileDelete(opts Opts) (bool, error) {
	logging.Debug(opts.logger, "reconciling delete pipeline")

	if len(opts.Resources) == 0 {
		return false, nil
	}

	if len(opts.Resources) > 1 {
		logging.Warn(opts.logger, "multiple delete pipelines found; only the first will be used")
	}

	pipeline := opts.Resources[0]
	isManualReconciliation := isManualReconciliation(opts.parentObject.GetLabels())
	mostRecentJob, err := getMostRecentDeletePipelineJob(opts, opts.namespace, pipeline)
	if err != nil {
		return false, err
	}

	if isManualReconciliation {
		logging.Info(opts.logger, "manual reconciliation detected for delete pipeline", "pipeline", pipeline.Name)
	}

	if isRunning(mostRecentJob) {
		if isManualReconciliation {
			logging.Info(opts.logger, "suspending job for manual reconciliation", "job", mostRecentJob.Name, "pipeline", pipeline.Name)
			if err = suspendJob(opts.ctx, opts.client, mostRecentJob); err != nil {
				logging.Error(opts.logger, err, "failed to suspend job", "job", mostRecentJob.GetName())
			}
			opts.eventRecorder.Eventf(opts.parentObject, "Normal", "PipelineSuspended", "Delete Pipeline suspended: %s", opts.Resources[0].Name)
			return true, err
		}

		logging.Debug(opts.logger, "job already inflight for pipeline; waiting for completion", "job", mostRecentJob.Name, "pipeline", pipeline.Name)
		return true, nil
	}

	if mostRecentJob == nil || isManualReconciliation {
		return createDeletePipeline(opts, pipeline)
	}

	logging.Debug(opts.logger, "checking status of delete pipeline")
	if mostRecentJob.Status.Succeeded > 0 {
		logging.Info(opts.logger, "delete pipeline completed")
		return false, nil
	}
	if mostRecentJob.Status.Failed > 0 {
		return false, ErrDeletePipelineFailed
	}

	logging.Debug(opts.logger, "delete pipeline still running", "status", mostRecentJob.Status)
	return true, nil
}

func createDeletePipeline(opts Opts, pipeline v1alpha1.PipelineJobResources) (passiveRequeue bool, err error) {
	logging.Debug(opts.logger, "creating delete pipeline; execution will commence")
	if isManualReconciliation(opts.parentObject.GetLabels()) {
		if err := removeManualReconciliationLabel(opts); err != nil {
			return false, err
		}
	}
	//TODO retrieve error information from applyResources to return to the caller
	applyResources(opts, append(pipeline.GetObjects(), pipeline.Job)...)
	opts.eventRecorder.Eventf(opts.parentObject, "Normal", "PipelineStarted", "Delete Pipeline started: %s", opts.Resources[0].Name)
	return true, nil
}

type configureObservedJobState string

const (
	configureObservedJobMissing   configureObservedJobState = "missing"
	configureObservedJobRunning   configureObservedJobState = "running"
	configureObservedJobSucceeded configureObservedJobState = "succeeded"
	configureObservedJobFailed    configureObservedJobState = "failed"
	configureObservedJobSuspended configureObservedJobState = "suspended"
)

type observedConfigurePipeline struct {
	index           int
	resource        v1alpha1.PipelineJobResources
	currentJob      *batchv1.Job
	currentJobState configureObservedJobState
}

type configureWorld struct {
	allJobs              []batchv1.Job
	pipelines            []observedConfigurePipeline
	manualReconcile      bool
	restartFromStart     bool
	resumeFromSuspended  bool
	suspendedPipelineIdx int
	currentPipelineIndex int
	completedCount       int64
	anomalousRunningJob  *batchv1.Job
}

func (w *configureWorld) currentPipeline() *observedConfigurePipeline {
	if w.currentPipelineIndex < 0 || w.currentPipelineIndex >= len(w.pipelines) {
		return nil
	}
	return &w.pipelines[w.currentPipelineIndex]
}

func (w *configureWorld) lastCompletedPipeline() *observedConfigurePipeline {
	if w.completedCount == 0 {
		return nil
	}

	lastCompletedIdx := int(w.completedCount) - 1
	if lastCompletedIdx < 0 || lastCompletedIdx >= len(w.pipelines) {
		return nil
	}

	return &w.pipelines[lastCompletedIdx]
}

func (w *configureWorld) currentPipelineFailed() bool {
	current := w.currentPipeline()
	if current == nil || w.manualReconcile || w.restartFromStart || w.resumeFromSuspended {
		return false
	}

	return current.currentJobState == configureObservedJobFailed || current.currentJobState == configureObservedJobSuspended
}

// ReconcileConfigure reconciles configure workflows.
// The returned bool is passiveRequeue:
// true means reconcile should happen again, passively, when watched external
// resources are updated (for example workflow Jobs or the parent object status),
// rather than by issuing an explicit direct requeue from this function.
func ReconcileConfigure(opts Opts) (passiveRequeue bool, err error) {
	if len(opts.Resources) == 0 {
		logging.Debug(opts.logger, "no pipeline resources to reconcile")
		return false, nil
	}

	world, err := ObserveConfigureWorld(opts)
	if err != nil {
		return false, err
	}

	if !opts.SkipConditions {
		if updated, blockAction, err := SyncConfigureStatus(opts, world); err != nil {
			return false, err
		} else if updated && blockAction {
			return true, nil
		}
	}

	return reconcileConfigureAction(opts, world)
}

func ObserveConfigureWorld(opts Opts) (*configureWorld, error) {
	world := &configureWorld{
		manualReconcile:      isManualReconciliation(opts.parentObject.GetLabels()),
		restartFromStart:     isWorkflowRestart(opts.parentObject.GetLabels()),
		suspendedPipelineIdx: -1,
		currentPipelineIndex: -1,
	}

	var err error
	world.suspendedPipelineIdx, err = resourceutil.GetSuspendedPipelineIndex(opts.parentObject)
	if err != nil {
		return nil, err
	}

	isWorkflowSuspended := opts.parentObject.GetLabels()[v1alpha1.WorkflowSuspendedLabel] == "true"
	world.resumeFromSuspended = !world.manualReconcile &&
		!world.restartFromStart &&
		!isWorkflowSuspended &&
		world.suspendedPipelineIdx >= 0

	allJobs, err := getJobsWithLabels(opts, labelsForJobs(opts), opts.namespace)
	if err != nil {
		logging.Error(opts.logger, err, "failed to list jobs")
		return nil, err
	}
	resourceutil.SortJobsByCreationDateTime(allJobs, false)
	world.allJobs = allJobs

	for i, pipeline := range opts.Resources {
		observedPipeline, err := observeConfigurePipeline(opts, i, pipeline)
		if err != nil {
			return nil, err
		}
		world.pipelines = append(world.pipelines, observedPipeline)
	}

	resolveConfigureCurrentPipeline(world)
	world.anomalousRunningJob = findConfigureAnomalousRunningJob(world)

	currentPipeline := world.currentPipeline()
	currentPipelineName := ""
	if currentPipeline != nil {
		currentPipelineName = currentPipeline.resource.Name
	}

	logging.Info(opts.logger, "observed configure world",
		"jobCount", len(world.allJobs),
		"currentPipeline", currentPipelineName,
		"completedCount", world.completedCount,
		"manualReconcile", world.manualReconcile,
		"restartFromStart", world.restartFromStart,
		"resumeFromSuspended", world.resumeFromSuspended,
		"suspendedPipelineIdx", world.suspendedPipelineIdx,
		"anomalousRunningJob", jobName(world.anomalousRunningJob))

	return world, nil
}

func observeConfigurePipeline(opts Opts, index int, pipeline v1alpha1.PipelineJobResources) (observedConfigurePipeline, error) {
	jobsForPipeline, err := getJobsWithLabels(opts, getLabelsForPipelineJob(pipeline), opts.namespace)
	if err != nil {
		logging.Error(opts.logger, err, "failed to list jobs for pipeline", "pipeline", pipeline.Name)
		return observedConfigurePipeline{}, err
	}

	resourceutil.SortJobsByCreationDateTime(jobsForPipeline, false)

	observedPipeline := observedConfigurePipeline{
		index:           index,
		resource:        pipeline,
		currentJobState: configureObservedJobMissing,
	}

	if len(jobsForPipeline) > 0 {
		observedPipeline.currentJob = &jobsForPipeline[0]
		observedPipeline.currentJobState = observeConfigureJobState(observedPipeline.currentJob)
	}

	logging.Debug(opts.logger, "observed configure pipeline",
		"pipeline", pipeline.Name,
		"pipelineIndex", index,
		"matchingJobCount", len(jobsForPipeline),
		"currentJob", jobName(observedPipeline.currentJob),
		"currentJobState", observedPipeline.currentJobState)

	return observedPipeline, nil
}

func observeConfigureJobState(job *batchv1.Job) configureObservedJobState {
	switch {
	case job == nil:
		return configureObservedJobMissing
	case isRunning(job):
		return configureObservedJobRunning
	case isSuspended(job):
		return configureObservedJobSuspended
	case isFailed(job):
		return configureObservedJobFailed
	default:
		return configureObservedJobSucceeded
	}
}

func resolveConfigureCurrentPipeline(world *configureWorld) {
	switch {
	case len(world.pipelines) == 0:
		world.currentPipelineIndex = -1
		world.completedCount = 0
	case world.manualReconcile || world.restartFromStart:
		world.currentPipelineIndex = 0
		world.completedCount = 0
	case world.resumeFromSuspended:
		world.currentPipelineIndex = world.suspendedPipelineIdx
		world.completedCount = int64(world.suspendedPipelineIdx)
	default:
		var previousCompletedAt time.Time
		previousCompletedSet := false
		for i := range world.pipelines {
			pipeline := world.pipelines[i]
			if pipeline.currentJobState != configureObservedJobSucceeded || pipeline.currentJob == nil {
				world.currentPipelineIndex = i
				return
			}

			completedAt := pipeline.currentJob.GetCreationTimestamp().Time
			if previousCompletedSet && completedAt.Before(previousCompletedAt) {
				world.pipelines[i].currentJobState = configureObservedJobMissing
				world.currentPipelineIndex = i
				return
			}

			world.completedCount++
			previousCompletedAt = completedAt
			previousCompletedSet = true
		}
		world.currentPipelineIndex = -1
	}
}

func findConfigureAnomalousRunningJob(world *configureWorld) *batchv1.Job {
	current := world.currentPipeline()
	allowedRunningJobName := ""
	if current != nil && current.currentJobState == configureObservedJobRunning && current.currentJob != nil {
		allowedRunningJobName = current.currentJob.Name
	}

	for i := range world.allJobs {
		job := &world.allJobs[i]
		if !isRunning(job) {
			continue
		}
		if allowedRunningJobName != "" && job.Name == allowedRunningJobName {
			continue
		}
		return job
	}

	return nil
}

func SyncConfigureStatus(opts Opts, world *configureWorld) (statusUpdated bool, blockAction bool, err error) {
	if updated, blockAction, err := syncConfigureResetStatus(opts, world); err != nil || updated {
		return updated, blockAction, err
	}

	if updated, blockAction, err := syncConfigureProgressStatus(opts, world); err != nil || updated {
		return updated, blockAction, err
	}

	if updated, blockAction, err := syncConfigureFailureStatus(opts, world); err != nil || updated {
		return updated, blockAction, err
	}

	if updated, blockAction, err := syncConfigureRunningStatus(opts, world); err != nil || updated {
		return updated, blockAction, err
	}

	return false, false, nil
}

func syncConfigureResetStatus(opts Opts, world *configureWorld) (bool, bool, error) {
	if !world.manualReconcile && !world.restartFromStart {
		return false, false, nil
	}

	currentSucceededCount := resourceutil.GetWorkflowsCounterStatus(opts.parentObject, "workflowsSucceeded")
	currentFailedCount := resourceutil.GetWorkflowsCounterStatus(opts.parentObject, "workflowsFailed")
	if currentSucceededCount == 0 && currentFailedCount == 0 && !configurePipelineStatusesNeedReset(opts.parentObject) {
		return false, false, nil
	}

	logging.Info(opts.logger, "resetting configure status before rerunning from pipeline 0",
		"manualReconcile", world.manualReconcile,
		"restartFromStart", world.restartFromStart)

	resourceutil.SetStatus(opts.parentObject, opts.logger, "workflowsSucceeded", int64(0), "workflowsFailed", int64(0))
	if err := resourceutil.ResetPipelineStatusToPending(opts.parentObject, opts.Resources); err != nil {
		return false, false, err
	}

	if err := opts.client.Status().Update(opts.ctx, opts.parentObject); err != nil {
		logging.Error(opts.logger, err, "failed to update parent object status")
		return false, false, err
	}

	return true, false, nil
}

func syncConfigureProgressStatus(opts Opts, world *configureWorld) (bool, bool, error) {
	currentSucceededCount := resourceutil.GetWorkflowsCounterStatus(opts.parentObject, "workflowsSucceeded")
	currentFailedCount := resourceutil.GetWorkflowsCounterStatus(opts.parentObject, "workflowsFailed")

	if currentSucceededCount == world.completedCount {
		if world.currentPipelineFailed() {
			return false, false, nil
		}
		if currentFailedCount == 0 {
			return false, false, nil
		}

		logging.Info(opts.logger, "resetting configure failed counter to match observed world",
			"currentFailedCount", currentFailedCount)
		resourceutil.SetStatus(opts.parentObject, opts.logger, "workflowsFailed", int64(0))
		if err := opts.client.Status().Update(opts.ctx, opts.parentObject); err != nil {
			logging.Error(opts.logger, err, "failed to update parent object status")
			return false, false, err
		}
		return true, true, nil
	}

	logging.Info(opts.logger, "syncing configure progress from observed world",
		"currentSucceededCount", currentSucceededCount,
		"completedCount", world.completedCount)

	resourceutil.SetStatus(opts.parentObject, opts.logger, "workflowsSucceeded", world.completedCount)
	if world.completedCount == 0 {
		resourceutil.SetStatus(opts.parentObject, opts.logger, "workflowsFailed", int64(0))
		if err := resourceutil.ResetPipelineStatusToPending(opts.parentObject, opts.Resources); err != nil {
			return false, false, err
		}
	} else {
		if currentFailedCount != 0 {
			resourceutil.SetStatus(opts.parentObject, opts.logger, "workflowsFailed", int64(0))
		}

		lastCompletedPipeline := world.lastCompletedPipeline()
		if lastCompletedPipeline != nil && lastCompletedPipeline.currentJob != nil {
			if err := resourceutil.MarkCurrentPipelineAsSucceeded(opts.parentObject, opts.logger, lastCompletedPipeline.currentJob); err != nil {
				logging.Error(opts.logger, err, "failed to mark current pipeline as succeeded")
				return false, false, err
			}
		}
	}

	if err := opts.client.Status().Update(opts.ctx, opts.parentObject); err != nil {
		logging.Error(opts.logger, err, "failed to update parent object status")
		return false, false, err
	}

	return true, true, nil
}

func syncConfigureFailureStatus(opts Opts, world *configureWorld) (bool, bool, error) {
	if !world.currentPipelineFailed() {
		return false, false, nil
	}

	currentPipeline := world.currentPipeline()
	if currentPipeline == nil || currentPipeline.currentJob == nil {
		return false, false, nil
	}

	expectedFailureMessage := fmt.Sprintf("A Configure Pipeline has failed: %s", currentPipeline.resource.Name)
	configureCondition := resourceutil.GetCondition(opts.parentObject, resourceutil.ConfigureWorkflowCompletedCondition)
	reconciledCondition := resourceutil.GetCondition(opts.parentObject, resourceutil.ReconciledCondition)
	currentFailedCount := resourceutil.GetWorkflowsCounterStatus(opts.parentObject, "workflowsFailed")
	currentPipelinePhase := getConfigurePipelinePhase(opts.parentObject, currentPipeline.resource.Name)

	needsUpdate := currentFailedCount != 1 ||
		currentPipelinePhase != v1alpha1.WorkflowPhaseFailed ||
		configureCondition == nil ||
		configureCondition.Status != v1.ConditionFalse ||
		configureCondition.Reason != resourceutil.ConfigureWorkflowCompletedFailedReason ||
		configureCondition.Message != expectedFailureMessage ||
		reconciledCondition == nil ||
		reconciledCondition.Status != v1.ConditionFalse ||
		reconciledCondition.Reason != resourceutil.ConfigureWorkflowCompletedFailedReason ||
		reconciledCondition.Message != "Failing"

	if !needsUpdate {
		return false, false, nil
	}

	logging.Warn(opts.logger, "syncing configure failure status from observed world",
		"pipeline", currentPipeline.resource.Name,
		"job", currentPipeline.currentJob.Name,
		"jobState", currentPipeline.currentJobState)

	resourceutil.SetStatus(opts.parentObject, opts.logger, "workflowsFailed", int64(1))
	if err := resourceutil.MarkCurrentPipelineAsFailed(opts.parentObject, opts.logger, currentPipeline.currentJob); err != nil {
		logging.Error(opts.logger, err, "failed to mark current pipeline as failed")
		return false, false, err
	}
	resourceutil.MarkConfigureWorkflowAsFailed(opts.logger, opts.parentObject, currentPipeline.resource.Name)
	resourceutil.MarkReconciledFailing(opts.parentObject, resourceutil.ConfigureWorkflowCompletedFailedReason)

	if err := opts.client.Status().Update(opts.ctx, opts.parentObject); err != nil {
		logging.Error(opts.logger, err, "failed to update parent object status")
		return false, false, err
	}

	return true, false, nil
}

func syncConfigureRunningStatus(opts Opts, world *configureWorld) (bool, bool, error) {
	currentPipeline := world.currentPipeline()
	if currentPipeline == nil || world.currentPipelineFailed() {
		return false, false, nil
	}

	configureCondition := resourceutil.GetCondition(opts.parentObject, resourceutil.ConfigureWorkflowCompletedCondition)
	reconciledCondition := resourceutil.GetCondition(opts.parentObject, resourceutil.ReconciledCondition)
	currentPipelinePhase := getConfigurePipelinePhase(opts.parentObject, currentPipeline.resource.Name)
	currentMessage := resourceutil.GetStatus(opts.parentObject, "message")

	needsUpdate := currentPipelinePhase != v1alpha1.WorkflowPhaseRunning ||
		(currentPipeline.index == 0 && (currentMessage == "" || currentMessage == "Resource requested")) ||
		configureCondition == nil ||
		configureCondition.Status != v1.ConditionFalse ||
		configureCondition.Reason != "PipelinesInProgress" ||
		configureCondition.Message != "Pipelines are still in progress" ||
		reconciledCondition == nil ||
		reconciledCondition.Status != v1.ConditionUnknown ||
		reconciledCondition.Reason != "WorkflowPending" ||
		reconciledCondition.Message != "Pending"

	if !needsUpdate {
		return false, false, nil
	}

	logging.Info(opts.logger, "syncing configure running status from observed world",
		"pipeline", currentPipeline.resource.Name,
		"pipelineIndex", currentPipeline.index,
		"jobState", currentPipeline.currentJobState)

	if currentPipeline.index == 0 && (currentMessage == "" || currentMessage == "Resource requested") {
		resourceutil.SetStatus(opts.parentObject, opts.logger, "message", "Pending")
	}

	resourceutil.MarkConfigureWorkflowAsRunning(opts.logger, opts.parentObject)
	resourceutil.MarkReconciledPending(opts.parentObject, "WorkflowPending")
	if err := resourceutil.MarkCurrentPipelineAsRunning(opts.parentObject, opts.logger, currentPipeline.resource.Job); err != nil {
		logging.Error(opts.logger, err, "failed to mark current pipeline as running")
		return false, false, err
	}

	if err := opts.client.Status().Update(opts.ctx, opts.parentObject); err != nil {
		logging.Error(opts.logger, err, "failed to update parent object status")
		return false, false, err
	}

	return true, false, nil
}

func reconcileConfigureAction(opts Opts, world *configureWorld) (bool, error) {
	if world.anomalousRunningJob != nil {
		logging.Info(opts.logger, "suspending anomalous running configure job",
			"job", world.anomalousRunningJob.Name,
			"status", overAllJobStatus(world.anomalousRunningJob))
		if err := suspendJob(opts.ctx, opts.client, world.anomalousRunningJob); err != nil {
			logging.Error(opts.logger, err, "failed to suspend job", "job", world.anomalousRunningJob.GetName())
			return true, err
		}
		return true, nil
	}

	currentPipeline := world.currentPipeline()
	if currentPipeline == nil {
		logging.Info(opts.logger, "all configure pipelines complete; cleaning up workflow resources")
		return false, cleanup(opts, opts.namespace)
	}

	opts.logger = opts.logger.WithName(currentPipeline.resource.Name).WithValues(
		"isManualReconciliation", world.manualReconcile,
		"isRestartFromStart", world.restartFromStart,
		"isResumeFromSuspended", world.resumeFromSuspended,
	)

	if currentPipeline.currentJobState == configureObservedJobRunning {
		if world.manualReconcile {
			logging.Info(opts.logger, "suspending running job for manual reconciliation",
				"job", currentPipeline.currentJob.Name,
				"pipeline", currentPipeline.resource.Name)
			if err := suspendJob(opts.ctx, opts.client, currentPipeline.currentJob); err != nil {
				logging.Error(opts.logger, err, "failed to suspend job", "job", currentPipeline.currentJob.GetName())
				return true, err
			}
			return true, nil
		}

		logging.Debug(opts.logger, "configure pipeline already running; waiting for completion",
			"job", currentPipeline.currentJob.Name,
			"pipeline", currentPipeline.resource.Name)
		return true, nil
	}

	if world.manualReconcile {
		logging.Info(opts.logger, "creating configure pipeline due to manual reconciliation",
			"pipeline", currentPipeline.resource.Name)
		return createConfigurePipeline(opts, currentPipeline.resource)
	}

	if world.restartFromStart {
		logging.Info(opts.logger, "creating configure pipeline due to run-from-start request",
			"pipeline", currentPipeline.resource.Name)
		return createConfigurePipeline(opts, currentPipeline.resource)
	}

	if world.resumeFromSuspended {
		logging.Info(opts.logger, fmt.Sprintf("rerunning suspended pipeline after %q is removed",
			v1alpha1.WorkflowSuspendedLabel), "pipeline", currentPipeline.resource.Name)
		return createConfigurePipeline(opts, currentPipeline.resource)
	}

	switch currentPipeline.currentJobState {
	case configureObservedJobMissing:
		logging.Info(opts.logger, "no current job found for configure pipeline; creating it",
			"pipeline", currentPipeline.resource.Name)
		return createConfigurePipeline(opts, currentPipeline.resource)
	case configureObservedJobFailed, configureObservedJobSuspended:
		opts.eventRecorder.Eventf(opts.parentObject, v1.EventTypeWarning,
			resourceutil.ConfigureWorkflowCompletedFailedReason, "A %s/configure Pipeline has failed: %s", opts.workflowType, currentPipeline.resource.Name)
		logging.Warn(opts.logger, "configure pipeline job failed; exiting workflow",
			"failedJob", currentPipeline.currentJob.Name,
			"pipeline", currentPipeline.resource.Name,
			"jobState", currentPipeline.currentJobState)
		return true, nil
	default:
		logging.Info(opts.logger, "configure world has no incomplete pipelines after status sync; cleaning up")
		return false, cleanup(opts, opts.namespace)
	}
}

func configurePipelineStatusesNeedReset(obj *unstructured.Unstructured) bool {
	workflows, found, err := unstructured.NestedSlice(obj.Object, "status", "kratix", "workflows", "pipelines")
	if err == nil && found {
		for _, workflow := range workflows {
			pipeline, ok := workflow.(map[string]any)
			if !ok {
				continue
			}

			if pipeline["phase"] != v1alpha1.WorkflowPhasePending {
				return true
			}
			if _, hasMessage := pipeline["message"]; hasMessage {
				return true
			}
		}
	}

	_, found, err = unstructured.NestedFieldNoCopy(obj.Object, "status", "kratix", "workflows", "suspendedGeneration")
	return err == nil && found
}

func getConfigurePipelinePhase(obj *unstructured.Unstructured, pipelineName string) string {
	workflows, found, err := unstructured.NestedSlice(obj.Object, "status", "kratix", "workflows", "pipelines")
	if err != nil || !found {
		return ""
	}

	for _, workflow := range workflows {
		pipeline, ok := workflow.(map[string]any)
		if !ok {
			continue
		}

		if pipeline["name"] != pipelineName {
			continue
		}

		phase, _ := pipeline["phase"].(string)
		return phase
	}

	return ""
}

func jobName(job *batchv1.Job) string {
	if job == nil {
		return ""
	}

	return job.Name
}

func suspendJob(ctx context.Context, c client.Client, job *batchv1.Job) error {
	trueBool := true
	patch := client.MergeFrom(job.DeepCopy())
	job.Spec.Suspend = &trueBool
	return c.Patch(ctx, job, patch)
}

func getLabelsForPipelineJob(pipeline v1alpha1.PipelineJobResources) map[string]string {
	return pipeline.Job.DeepCopy().GetLabels()
}

func labelsForJobs(opts Opts) map[string]string {
	l := map[string]string{
		v1alpha1.WorkflowTypeLabel: opts.workflowType,
	}
	promiseName := opts.parentObject.GetName()
	if strings.HasPrefix(opts.workflowType, string(v1alpha1.WorkflowTypeResource)) {
		promiseName = opts.parentObject.GetLabels()[v1alpha1.PromiseNameLabel]
		l[v1alpha1.ResourceNameLabel] = opts.parentObject.GetName()
		if opts.namespace != opts.parentObject.GetNamespace() {
			// only set resource request namespace label when workflow running in different namespace from the resource requests
			l[v1alpha1.ResourceNamespaceLabel] = opts.parentObject.GetNamespace()
		}
	}
	l[v1alpha1.PromiseNameLabel] = promiseName
	return l
}

func labelsForAllWorkflowJobs(pipeline v1alpha1.PipelineJobResources) map[string]string {
	pipelineLabels := pipeline.Job.GetLabels()
	labels := map[string]string{
		v1alpha1.PromiseNameLabel: pipelineLabels[v1alpha1.PromiseNameLabel],
	}
	if pipelineLabels[v1alpha1.ResourceNameLabel] != "" {
		labels[v1alpha1.ResourceNameLabel] = pipelineLabels[v1alpha1.ResourceNameLabel]
	}
	if pipelineLabels[v1alpha1.ResourceNamespaceLabel] != "" {
		labels[v1alpha1.ResourceNamespaceLabel] = pipelineLabels[v1alpha1.ResourceNamespaceLabel]
	}
	if pipelineLabels[v1alpha1.WorkflowActionLabel] != "" {
		labels[v1alpha1.WorkflowActionLabel] = pipelineLabels[v1alpha1.WorkflowActionLabel]
	}
	if pipelineLabels[v1alpha1.WorkflowTypeLabel] != "" {
		labels[v1alpha1.WorkflowTypeLabel] = pipelineLabels[v1alpha1.WorkflowTypeLabel]
	}
	return labels
}

func jobIsForPipeline(pipeline v1alpha1.PipelineJobResources, job *batchv1.Job) bool {
	if job == nil {
		return false
	}

	jobLabels := job.GetLabels()
	pipelineLabels := pipeline.Job.GetLabels()

	if jobLabels[v1alpha1.KratixResourceHashLabel] != pipelineLabels[v1alpha1.KratixResourceHashLabel] {
		return false
	}

	if jobLabels[v1alpha1.WorkflowTypeLabel] != pipelineLabels[v1alpha1.WorkflowTypeLabel] {
		return false
	}

	if jobLabels[v1alpha1.WorkflowActionLabel] != pipelineLabels[v1alpha1.WorkflowActionLabel] {
		return false
	}

	if jobLabels[v1alpha1.KratixPipelineHashLabel] != pipelineLabels[v1alpha1.KratixPipelineHashLabel] {
		return false
	}

	return jobLabels[v1alpha1.PipelineNameLabel] == pipelineLabels[v1alpha1.PipelineNameLabel]
}

func isFailed(job *batchv1.Job) bool {
	if job == nil {
		return false
	}

	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobFailed || condition.Type == batchv1.JobSuspended {
			return true
		}
	}
	return false
}

func isSuspended(job *batchv1.Job) bool {
	if job == nil {
		return false
	}

	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobSuspended && condition.Status == v1.ConditionTrue {
			return true
		}
	}

	return false
}

func isCompleted(job *batchv1.Job) bool {
	return !isRunning(job) && !isFailed(job)
}

func isRunning(job *batchv1.Job) bool {
	if job == nil {
		return false
	}

	if job.Status.Active > 0 {
		return true
	}

	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobComplete || condition.Type == batchv1.JobSuspended || condition.Type == batchv1.JobFailed {
			return false
		}
	}
	return true
}

func cleanup(opts Opts, namespace string) error {
	pipelineNames := map[string]bool{}
	for _, pipeline := range opts.Resources {
		l := labelsForAllWorkflowJobs(pipeline)
		l[v1alpha1.PipelineNameLabel] = pipeline.Name
		pipelineNames[pipeline.Name] = true
		jobsForPipeline, _ := getJobsWithLabels(opts, l, namespace)
		if err := cleanupJobs(opts, jobsForPipeline); err != nil {
			logging.Error(opts.logger, err, "failed to delete old jobs")
			return err
		}
	}

	allPipelineWorks, err := resourceutil.GetWorksByType(opts.client, v1alpha1.Type(opts.workflowType), opts.parentObject)
	if err != nil {
		logging.Error(opts.logger, err, "failed to list works for Promise", "promise", opts.parentObject.GetName())
		return err
	}
	for _, work := range allPipelineWorks {
		workPipelineName := work.GetLabels()[v1alpha1.PipelineNameLabel]
		if !pipelineNames[workPipelineName] {
			logging.Debug(opts.logger, "deleting old work", "work", work.GetName(), "objectName", opts.parentObject.GetName(), "workType", work.Labels[v1alpha1.WorkTypeLabel])
			if err := opts.client.Delete(opts.ctx, &work); err != nil {
				logging.Error(opts.logger, err, "failed to delete old work", "work", work.GetName())
				return err
			}

		}
	}

	return nil
}

func cleanupJobs(opts Opts, pipelineJobsAtCurrentSpec []batchv1.Job) error {
	if len(pipelineJobsAtCurrentSpec) <= opts.numberOfJobsToKeep {
		logging.Debug(opts.logger,
			"pipeline jobs at current spec do not exceed number of jobs to keep",
			"numberOfJobsToKeep", opts.numberOfJobsToKeep,
			"number of pipeline jobs at current spec", len(pipelineJobsAtCurrentSpec))
		return nil
	}

	// Sort jobs by creation time
	pipelineJobsAtCurrentSpec = resourceutil.SortJobsByCreationDateTime(pipelineJobsAtCurrentSpec, true)

	// Delete all but the last n jobs; n defaults to 5 and can be configured by env var for the operator
	for i := 0; i < len(pipelineJobsAtCurrentSpec)-opts.numberOfJobsToKeep; i++ {
		job := pipelineJobsAtCurrentSpec[i]
		logging.Debug(opts.logger,
			"deleting old job",
			"name", job.GetName(),
			"labels", job.GetLabels(),
			"createdTimestamp", job.GetCreationTimestamp().Time,
			"status", overAllJobStatus(&job))
		if err := opts.client.Delete(opts.ctx, &job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
			if !errors.IsNotFound(err) {
				logging.Warn(opts.logger, "failed to delete job; will retry", "job", job.GetName(), "error", err)
				return nil
			}
		}
	}

	return nil
}

func createConfigurePipeline(opts Opts, resources v1alpha1.PipelineJobResources) (passiveRequeue bool, err error) {
	logging.Info(opts.logger, "triggering pipeline", "workflowAction", resources.WorkflowAction)
	var objectToDelete []client.Object
	if objectToDelete, err = getObjectsToDelete(opts, resources); err != nil {
		return false, err
	}

	logging.Trace(opts.logger, "reconciling for parent object", "parent", opts.parentObject.GetName())
	if isManualReconciliation(opts.parentObject.GetLabels()) {
		if err := removeManualReconciliationLabel(opts); err != nil {
			return false, err
		}
	}
	if isWorkflowRestart(opts.parentObject.GetLabels()) {
		if err := removeWorkflowRestartLabel(opts); err != nil {
			return false, err
		}
	}

	deleteResources(opts, objectToDelete...)
	applyResources(opts, append(resources.GetObjects(), resources.Job)...)

	opts.eventRecorder.Eventf(opts.parentObject, "Normal", "PipelineStarted", "Configure Pipeline started: %s", resources.Name)

	return true, nil
}

func removeManualReconciliationLabel(opts Opts) error {
	logging.Debug(opts.logger, "manual reconciliation label detected; removing it")
	return removeLabel(opts, resourceutil.ManualReconciliationLabel)
}

func removeWorkflowRestartLabel(opts Opts) error {
	logging.Debug(opts.logger, "workflow restart label detected; removing it")
	return removeLabel(opts, resourceutil.WorkflowRunFromStartLabel)
}

func removeLabel(opts Opts, labelKey string) error {
	newLabels := opts.parentObject.GetLabels()
	delete(newLabels, labelKey)
	opts.parentObject.SetLabels(newLabels)
	if err := opts.client.Update(opts.ctx, opts.parentObject); err != nil {
		logging.Error(opts.logger, err, "failed to remove label", "label", labelKey)
		return err
	}
	return nil
}

func getMostRecentDeletePipelineJob(opts Opts, namespace string, pipeline v1alpha1.PipelineJobResources) (*batchv1.Job, error) {
	labels := getLabelsForPipelineJob(pipeline)
	jobs, err := getJobsWithLabels(opts, labels, namespace)
	if err != nil || len(jobs) == 0 {
		return nil, err
	}
	resourceutil.SortJobsByCreationDateTime(jobs, false)
	return &jobs[0], nil
}

func getJobsWithLabels(opts Opts, jobLabels map[string]string, namespace string) ([]batchv1.Job, error) {
	selectorLabels := labels.FormatLabels(jobLabels)
	selector, err := labels.Parse(selectorLabels)

	if err != nil {
		return nil, fmt.Errorf("error parsing labels %v: %w", jobLabels, err)
	}

	listOps := &client.ListOptions{
		LabelSelector: selector,
		Namespace:     namespace,
	}

	jobs := &batchv1.JobList{}
	err = opts.client.List(opts.ctx, jobs, listOps)
	if err != nil {
		logging.Error(opts.logger, err, "error listing jobs", "selectors", selector.String())
		return nil, err
	}
	return jobs.Items, nil
}

func isManualReconciliation(labels map[string]string) bool {
	return isLabelSetToTrue(labels, resourceutil.ManualReconciliationLabel)
}

func isWorkflowRestart(labels map[string]string) bool {
	return isLabelSetToTrue(labels, resourceutil.WorkflowRunFromStartLabel)
}

func isLabelSetToTrue(labels map[string]string, labelKey string) bool {
	if labels == nil {
		return false
	}
	val, exists := labels[labelKey]
	return exists && val == "true"
}

// TODO return error info (summary of errors from resources?) to the caller, instead of just logging
func applyResources(opts Opts, resources ...client.Object) {
	logging.Debug(opts.logger, "reconciling pipeline resources")

	for _, resource := range resources {
		logger := opts.logger.WithValues("type", reflect.TypeOf(resource), "gvk", resource.GetObjectKind().GroupVersionKind().String(), "name", resource.GetName(), "namespace", resource.GetNamespace(), "labels", resource.GetLabels())

		logging.Debug(logger, "reconciling resource")
		if err := opts.client.Create(opts.ctx, resource); err != nil {
			if errors.IsAlreadyExists(err) {
				if resource.GetObjectKind().GroupVersionKind().Kind == rbacv1.ServiceAccountKind {
					serviceAccount := &v1.ServiceAccount{}
					if err := opts.client.Get(opts.ctx, client.ObjectKey{Namespace: resource.GetNamespace(), Name: resource.GetName()}, serviceAccount); err != nil {
						logging.Error(logger, err, "error getting service account")
						continue
					}

					if _, ok := serviceAccount.Labels[v1alpha1.PromiseNameLabel]; !ok {
						logging.Debug(opts.logger, "service account exists but was not created by kratix; skipping update", "name", serviceAccount.GetName(), "namespace", serviceAccount.GetNamespace(), "labels", serviceAccount.GetLabels())
						continue
					}

				}
				logging.Debug(logger, "resource already exists; updating")
				if err = opts.client.Update(opts.ctx, resource); err == nil {
					continue
				}
			}

			logging.Error(logger, err, "error reconciling resource")
			y, _ := yaml.Marshal(&resource)
			logging.Error(logger, err, string(y))
		} else {
			logging.Debug(logger, "resource created")
		}
	}

	time.Sleep(minimumPeriodBetweenCreatingPipelineResources)
}

func deleteResources(opts Opts, resources ...client.Object) {
	for _, resource := range resources {
		logger := opts.logger.WithValues("type", reflect.TypeOf(resource), "gvk", resource.GetObjectKind().GroupVersionKind().String(), "name", resource.GetName(), "namespace", resource.GetNamespace(), "labels", resource.GetLabels())
		logging.Debug(logger, "deleting resource")
		if err := opts.client.Delete(opts.ctx, resource); err != nil {
			if errors.IsNotFound(err) {
				logging.Debug(logger, "resource already deleted")
				continue
			}
			logging.Error(logger, err, "error deleting resource")
			y, _ := yaml.Marshal(&resource)
			logging.Error(logger, err, string(y))
		} else {
			logging.Debug(logger, "resource deleted")
		}
	}
}

// overAllJobStatus returns job status as 'Running', 'Completed', 'Suspended', or 'Failed'
func overAllJobStatus(job *batchv1.Job) string {
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobComplete && condition.Status == v1.ConditionTrue {
			return string(condition.Type)
		}

		if condition.Type == batchv1.JobSuspended && condition.Status == v1.ConditionTrue {
			return string(condition.Type)
		}

		if condition.Type == batchv1.JobFailed && condition.Status == v1.ConditionTrue {
			return string(condition.Type)
		}
	}
	return "Running"
}
