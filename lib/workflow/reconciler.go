package workflow

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	"github.com/syntasso/kratix/api/v1alpha1"
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
	Pipelines []Pipeline
	source    string
}

type Pipeline struct {
	Name string
	Job  *batchv1.Job
	// ServiceAccount, Role, Rolebinding, ConfigMap etc (differs for delete vs configure)
	JobRequiredResources []client.Object
}

func NewOpts(ctx context.Context, client client.Client, logger logr.Logger, parentObj *unstructured.Unstructured, pipelines []Pipeline, source string) Opts {
	return Opts{
		ctx:          ctx,
		client:       client,
		logger:       logger,
		parentObject: parentObj,
		source:       source,
		Pipelines:    pipelines,
	}
}

// TODO refactor
func ReconcileDelete(opts Opts) (bool, error) {
	opts.logger.Info("Reconciling Delete Pipeline")

	if len(opts.Pipelines) == 0 {
		return false, nil
	}

	if len(opts.Pipelines) > 1 {
		opts.logger.Info("Multiple delete pipeline found but only one delete pipeline is currently supported. Ignoring all but the first")
	}

	pipeline := opts.Pipelines[0]
	existingDeletePipeline, err := getDeletePipeline(opts, opts.parentObject.GetNamespace(), pipeline)
	if err != nil {
		return false, err
	}

	if existingDeletePipeline == nil {
		opts.logger.Info("Creating Delete Pipeline. The pipeline will now execute...")

		//TODO retrieve error information from applyResources to return to the caller
		applyResources(opts, append(pipeline.JobRequiredResources, pipeline.Job)...)

		return true, nil
	}

	opts.logger.Info("Checking status of Delete Pipeline")
	if existingDeletePipeline.Status.Succeeded > 0 {
		opts.logger.Info("Delete Pipeline Completed")
		return false, nil
	}

	opts.logger.Info("Delete Pipeline not finished", "status", existingDeletePipeline.Status)
	return true, nil
}

func ReconcileConfigure(opts Opts) (bool, error) {
	originalLogger := opts.logger
	namespace := opts.parentObject.GetNamespace()
	if namespace == "" {
		namespace = v1alpha1.SystemNamespace
	}

	l := labelsForJobs(opts)
	allJobs, err := getJobsWithLabels(opts, l, namespace)
	if err != nil {
		opts.logger.Error(err, "failed to list jobs")
		return false, err
	}

	var pipelineIndex = 0
	var mostRecentJob *batchv1.Job

	if len(allJobs) != 0 {
		resourceutil.SortJobsByCreationDateTime(allJobs, false)
		mostRecentJob = &allJobs[0]
		pipelineIndex = nextPipelineIndex(opts, mostRecentJob)
	}

	if pipelineIndex >= len(opts.Pipelines) {
		pipelineIndex = len(opts.Pipelines) - 1
	}

	if pipelineIndex < 0 {
		opts.logger.Info("No pipeline to reconcile")
		return false, nil
	}

	var mostRecentJobName = "n/a"
	if mostRecentJob != nil {
		mostRecentJobName = mostRecentJob.Name
	}

	opts.logger.Info("Reconciling Configure workflow", "pipelineIndex", pipelineIndex, "mostRecentJob", mostRecentJobName)

	pipeline := opts.Pipelines[pipelineIndex]
	opts.logger = originalLogger.WithName(pipeline.Name)

	if jobIsForPipeline(pipeline, mostRecentJob) {
		if isRunning(mostRecentJob) {
			opts.logger.Info("Job already inflight for Pipeline, waiting for it to complete", "job", mostRecentJob.Name, "pipeline", pipeline.Name)
			return true, nil
		}

		if isManualReconciliation(opts.parentObject.GetLabels()) {
			opts.logger.Info("Pipeline running due to manual reconciliation", "pipeline", pipeline.Name)
			return createConfigurePipeline(opts, pipelineIndex, pipeline)
		}

		if isFailed(mostRecentJob) {
			opts.logger.Info("Last Job for Pipeline has failed, exiting workflow", "failedJob", mostRecentJob.Name, "pipeline", pipeline.Name)
			return false, nil
		}

		if err := cleanup(opts, namespace); err != nil {
			return false, err
		}

		return false, nil
	}

	// TODO this will suspend any job that is in flight (without checking if it's active)
	// and the next pipeline will immediately be started - this may be okay, but is
	// different to how things used to be (where we only suspended a job if it didn't
	// have any active pods)
	if isRunning(mostRecentJob) {
		opts.logger.Info("Job already inflight for another workflow, suspending it", "job", mostRecentJob.Name)
		trueBool := true
		patch := client.MergeFrom(mostRecentJob.DeepCopy())
		mostRecentJob.Spec.Suspend = &trueBool
		err := opts.client.Patch(opts.ctx, mostRecentJob, patch)
		if err != nil {
			opts.logger.Error(err, "failed to patch Job", "job", mostRecentJob.GetName())
		}
		return true, nil
	}

	// TODO this will be very noisy - might want to slowRequeue?
	opts.logger.Info("Reconciling pipeline", "pipeline", pipeline.Name)
	return createConfigurePipeline(opts, pipelineIndex, pipeline)
}

func getLabelsForPipelineJob(pipeline Pipeline) map[string]string {
	labels := pipeline.Job.DeepCopy().GetLabels()
	return labels
}

func labelsForJobs(opts Opts) map[string]string {
	l := map[string]string{
		v1alpha1.WorkTypeLabel: opts.source,
	}
	promiseName := opts.parentObject.GetName()
	if opts.source == string(v1alpha1.WorkflowTypeResource) {
		promiseName = opts.parentObject.GetLabels()[v1alpha1.PromiseNameLabel]
		l[v1alpha1.ResourceNameLabel] = opts.parentObject.GetName()
	}
	l[v1alpha1.PromiseNameLabel] = promiseName
	return l
}

func labelsForAllPipelineJobs(pipeline Pipeline) map[string]string {
	pipelineLabels := pipeline.Job.GetLabels()
	labels := map[string]string{
		v1alpha1.PromiseNameLabel: pipelineLabels[v1alpha1.PromiseNameLabel],
	}
	if pipelineLabels[v1alpha1.ResourceNameLabel] != "" {
		labels[v1alpha1.ResourceNameLabel] = pipelineLabels[v1alpha1.ResourceNameLabel]
	}
	return labels
}

func jobIsForPipeline(pipeline Pipeline, job *batchv1.Job) bool {
	if job == nil {
		return false
	}

	if job.GetLabels()[v1alpha1.KratixResourceHashLabel] != pipeline.Job.GetLabels()[v1alpha1.KratixResourceHashLabel] {
		return false
	}

	return job.GetLabels()[v1alpha1.PipelineNameLabel] == pipeline.Job.GetLabels()[v1alpha1.PipelineNameLabel]
}

func nextPipelineIndex(opts Opts, mostRecentJob *batchv1.Job) int {
	if mostRecentJob == nil {
		return 0
	}

	if isManualReconciliation(opts.parentObject.GetLabels()) {
		return 0
	}

	i := len(opts.Pipelines) - 1
	for i >= 0 {
		if jobIsForPipeline(opts.Pipelines[i], mostRecentJob) {
			opts.logger.Info("Found job for pipeline", "pipeline", opts.Pipelines[i].Name, "index", i)
			if isFailed(mostRecentJob) || isRunning(mostRecentJob) {
				return i
			}
			break
		}
		i -= 1
	}

	opts.logger.Info("Next pipeline is", "index", i+1)
	return i + 1
}

func isFailed(job *batchv1.Job) bool {
	if job == nil {
		return false
	}

	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobFailed {
			return true
		}
	}
	return false
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
	if opts.source == "promise" {
		for _, pipeline := range opts.Pipelines {
			if err := deleteConfigMap(opts, pipeline); err != nil {
				return err
			}
		}
	}

	pipelineNames := map[string]bool{}
	for _, pipeline := range opts.Pipelines {
		l := labelsForAllPipelineJobs(pipeline)
		l[v1alpha1.PipelineNameLabel] = pipeline.Name
		pipelineNames[pipeline.Name] = true
		jobsForPipeline, _ := getJobsWithLabels(opts, l, namespace)
		// TODO: come back to this and reason about it
		if err := deleteAllButLastFiveJobs(opts, jobsForPipeline); err != nil {
			opts.logger.Error(err, "failed to delete old jobs")
			return err
		}
	}

	allPipelineWorks, err := resourceutil.GetWorksByType(opts.client, v1alpha1.Type(opts.source), opts.parentObject)
	if err != nil {
		opts.logger.Error(err, "failed to list works for Promise", "promise", opts.parentObject.GetName())
		return err
	}
	for _, work := range allPipelineWorks {
		workPipelineName := work.GetLabels()[v1alpha1.PipelineNameLabel]
		if !pipelineNames[workPipelineName] {
			opts.logger.Info("Deleting old work", "work", work.GetName(), "objectName", opts.parentObject.GetName(), "workType", work.Labels[v1alpha1.WorkTypeLabel])
			if err := opts.client.Delete(opts.ctx, &work); err != nil {
				opts.logger.Error(err, "failed to delete old work", "work", work.GetName())
				return err
			}

		}
	}

	return nil
}

const numberOfJobsToKeep = 5

func deleteAllButLastFiveJobs(opts Opts, pipelineJobsAtCurrentSpec []batchv1.Job) error {
	if len(pipelineJobsAtCurrentSpec) <= numberOfJobsToKeep {
		return nil
	}

	// Sort jobs by creation time
	pipelineJobsAtCurrentSpec = resourceutil.SortJobsByCreationDateTime(pipelineJobsAtCurrentSpec, true)

	// Delete all but the last 5 jobs
	for i := 0; i < len(pipelineJobsAtCurrentSpec)-numberOfJobsToKeep; i++ {
		job := pipelineJobsAtCurrentSpec[i]
		opts.logger.Info("Deleting old job", "job", job.GetName())
		if err := opts.client.Delete(opts.ctx, &job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
			if !errors.IsNotFound(err) {
				opts.logger.Info("failed to delete job", "job", job.GetName(), "error", err)
				return nil
			}
		}
	}

	return nil
}

func deleteConfigMap(opts Opts, pipeline Pipeline) error {
	configMap := &v1.ConfigMap{}
	for _, resource := range pipeline.JobRequiredResources {
		if _, ok := resource.(*v1.ConfigMap); ok {
			configMap = resource.(*v1.ConfigMap)
			break
		}
	}

	opts.logger.Info("Removing configmap", "name", configMap.GetName())
	if err := opts.client.Delete(opts.ctx, configMap); err != nil {
		if !errors.IsNotFound(err) {
			opts.logger.Info("failed to delete configmap", "name", configMap.GetName(), "error", err)
			return err
		}
	}

	return nil
}

func createConfigurePipeline(opts Opts, pipelineIndex int, pipeline Pipeline) (bool, error) {
	updated, err := setPipelineCompletedConditionStatus(opts, pipelineIndex == 0, opts.parentObject)
	if err != nil || updated {
		return updated, err
	}

	opts.logger.Info("Triggering Promise pipeline")

	applyResources(opts, append(pipeline.JobRequiredResources, pipeline.Job)...)

	opts.logger.Info("Parent object:", "parent", opts.parentObject.GetName())
	if isManualReconciliation(opts.parentObject.GetLabels()) {
		if err := removeManualReconciliationLabel(opts); err != nil {
			return false, err
		}
		return false, nil
	}

	return true, nil
}

func removeManualReconciliationLabel(opts Opts) error {
	opts.logger.Info("Manual reconciliation label detected; removing it")
	newLabels := opts.parentObject.GetLabels()
	delete(newLabels, resourceutil.ManualReconciliationLabel)
	opts.parentObject.SetLabels(newLabels)
	if err := opts.client.Update(opts.ctx, opts.parentObject); err != nil {
		opts.logger.Error(err, "couldn't remove the label...")
		return err
	}
	return nil
}

func setPipelineCompletedConditionStatus(opts Opts, isTheFirstPipeline bool, obj *unstructured.Unstructured) (bool, error) {
	switch resourceutil.GetPipelineCompletedConditionStatus(obj) {
	case v1.ConditionTrue:
		fallthrough
	case v1.ConditionUnknown:
		currentMessage := resourceutil.GetStatus(obj, "message")
		if isTheFirstPipeline || currentMessage == "" || currentMessage == "Resource requested" {
			resourceutil.SetStatus(obj, opts.logger, "message", "Pending")
		}
		resourceutil.MarkPipelineAsRunning(opts.logger, obj)
		err := opts.client.Status().Update(opts.ctx, obj)
		if err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

func getDeletePipeline(opts Opts, namespace string, pipeline Pipeline) (*batchv1.Job, error) {
	labels := getLabelsForPipelineJob(pipeline)
	jobs, err := getJobsWithLabels(opts, labels, namespace)
	if err != nil || len(jobs) == 0 {
		return nil, err
	}
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
		opts.logger.Error(err, "error listing jobs", "selectors", selector.String())
		return nil, err
	}
	return jobs.Items, nil
}

func isManualReconciliation(labels map[string]string) bool {
	if labels == nil {
		return false
	}
	val, exists := labels[resourceutil.ManualReconciliationLabel]
	return exists && val == "true"
}

// TODO return error info (summary of errors from resources?) to the caller, instead of just logging
func applyResources(opts Opts, resources ...client.Object) {
	opts.logger.Info("Reconciling pipeline resources")

	for _, resource := range resources {
		logger := opts.logger.WithValues("gvk", resource.GetObjectKind().GroupVersionKind(), "name", resource.GetName(), "namespace", resource.GetNamespace(), "labels", resource.GetLabels())

		logger.Info("Reconciling")
		if err := opts.client.Create(opts.ctx, resource); err != nil {
			logger.Info("Creating resources", "resources", resource)
			if errors.IsAlreadyExists(err) {
				logger.Info("Resource already exists, will update")
				if err = opts.client.Update(opts.ctx, resource); err == nil {
					continue
				}
			}

			logger.Error(err, "Error reconciling on resource")
			y, _ := yaml.Marshal(&resource)
			logger.Error(err, string(y))
		} else {
			logger.Info("Resource created")
		}
	}
}
