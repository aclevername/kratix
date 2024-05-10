package pipeline

import (
	"fmt"
	"os"
	"strings"

	"github.com/go-logr/logr"
	"github.com/syntasso/kratix/api/v1alpha1"
	"github.com/syntasso/kratix/lib/hash"
	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func NewConfigureResource(
	rr *unstructured.Unstructured,
	promise *unstructured.Unstructured,
	crdPlural string,
	pipeline v1alpha1.Pipeline,
	resourceRequestIdentifier,
	promiseIdentifier string,
	promiseDestinationSelectors []v1alpha1.PromiseScheduling,
	logger logr.Logger,
) ([]client.Object, error) {

	pipelineResources := NewPipelineArgs(promiseIdentifier, resourceRequestIdentifier, pipeline.Name, rr.GetName(), rr.GetNamespace())
	destinationSelectorsConfigMap, err := destinationSelectorsConfigMap(pipelineResources, promiseDestinationSelectors, nil)
	if err != nil {
		return nil, err
	}

	promiseHash, err := hash.ComputeHashForResource(promise)
	if err != nil {
		return nil, err
	}

	objHash, err := hash.ComputeHashForResource(rr)
	if err != nil {
		return nil, err
	}

	combinedHash := hash.ComputeHash(fmt.Sprintf("%s-%s", promiseHash, objHash))

	job, err := ConfigurePipeline(rr, combinedHash, pipeline, pipelineResources, promiseIdentifier, false, logger)
	if err != nil {
		return nil, err
	}

	resources := []client.Object{
		serviceAccount(pipelineResources),
		role(rr, crdPlural, pipelineResources),
		roleBinding(pipelineResources),
		destinationSelectorsConfigMap,
		job,
	}

	return resources, nil
}

func NewConfigurePromise(
	uPromise *unstructured.Unstructured,
	p v1alpha1.Pipeline,
	promiseIdentifier string,
	promiseDestinationSelectors []v1alpha1.PromiseScheduling,
	logger logr.Logger,
) ([]client.Object, error) {

	pipelineResources := NewPipelineArgs(promiseIdentifier, "", p.Name, uPromise.GetName(), v1alpha1.SystemNamespace)
	destinationSelectorsConfigMap, err := destinationSelectorsConfigMap(pipelineResources, promiseDestinationSelectors, nil)
	if err != nil {
		return nil, err
	}

	objHash, err := hash.ComputeHashForResource(uPromise)
	if err != nil {
		return nil, err
	}

	pipeline, err := ConfigurePipeline(uPromise, objHash, p, pipelineResources, promiseIdentifier, true, logger)
	if err != nil {
		return nil, err
	}

	resources := []client.Object{
		serviceAccount(pipelineResources),
		clusterRole(pipelineResources),
		clusterRoleBinding(pipelineResources),
		destinationSelectorsConfigMap,
		pipeline,
	}

	return resources, nil
}

func NewDestinationPipeline(
	uWork *unstructured.Unstructured,
	destination string,
	p v1alpha1.Pipeline,
	promiseIdentifier string,
	logger logr.Logger,
) ([]client.Object, error) {

	pipelineResources := NewPipelineArgs(promiseIdentifier, "", p.Name, uWork.GetName(), v1alpha1.SystemNamespace)
	pipelineResources.names["destination"] = "destination"
	destinationSelectorsConfigMap, err := destinationSelectorsConfigMap(pipelineResources, nil, nil)
	if err != nil {
		return nil, err
	}

	objHash, err := hash.ComputeHashForResource(uWork)
	if err != nil {
		return nil, err
	}

	pipeline, err := ConfigurePipeline(uWork, objHash, p, pipelineResources, promiseIdentifier, true, logger)
	if err != nil {
		return nil, err
	}
	//Current:
	//init:
	//0: get work
	//1: user
	//2: work writer
	//container:
	//update status

	//Desired:
	//init:
	//0: get work
	//1: user
	//container:
	//workplacement writer

	//remove just the last init container
	initContainers := []v1.Container{}
	count := len(pipeline.Spec.Template.Spec.InitContainers)
	for i := range count {
		if i == count-1 {
			continue
		}
		initContainers = append(initContainers, pipeline.Spec.Template.Spec.InitContainers[i])
	}
	pipeline.Spec.Template.Spec.InitContainers = initContainers

	//inject workplacement writer
	pipeline.Spec.Template.Spec.Containers[0].Name = "workplacement-writer"
	pipeline.Spec.Template.Spec.Containers[0].Command = []string{"sh", "-c", "create-workplacement"}
	pipeline.Spec.Template.Spec.Containers[0].Env = append(
		pipeline.Spec.Template.Spec.Containers[0].Env,
		v1.EnvVar{
			Name:  "DESTINATION_NAME",
			Value: destination,
		},
	)

	pipeline.Labels["kratix.io/work-type"] = "promise-destination"
	pipeline.Labels["kratix.io/promise-name"] = uWork.GetName()
	pipeline.Name = strings.Replace(pipeline.Name, "configure-", "destination-", 1)

	resources := []client.Object{
		serviceAccount(pipelineResources),
		clusterRole(pipelineResources),
		clusterRoleBinding(pipelineResources),
		destinationSelectorsConfigMap,
		pipeline,
	}

	return resources, nil
}

func ConfigurePipeline(obj *unstructured.Unstructured, objHash string, pipeline v1alpha1.Pipeline, pipelineArgs PipelineArgs, promiseName string, promiseWorkflow bool, logger logr.Logger) (*batchv1.Job, error) {

	volumes := metadataAndSchedulingVolumes(pipelineArgs.ConfigMapName())

	initContainers, pipelineVolumes := generateConfigurePipelineContainersAndVolumes(obj, pipeline, promiseName, promiseWorkflow, logger)
	volumes = append(volumes, pipelineVolumes...)

	objHash, err := hash.ComputeHashForResource(obj)
	if err != nil {
		return nil, err
	}

	var imagePullSecrets []v1.LocalObjectReference
	workCreatorPullSecrets := os.Getenv("WC_PULL_SECRET")
	if workCreatorPullSecrets != "" {
		imagePullSecrets = append(imagePullSecrets, v1.LocalObjectReference{Name: workCreatorPullSecrets})
	}

	imagePullSecrets = append(imagePullSecrets, pipeline.Spec.ImagePullSecrets...)

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pipelineArgs.ConfigurePipelineName(),
			Namespace: pipelineArgs.Namespace(),
			Labels:    pipelineArgs.ConfigurePipelineJobLabels(objHash),
		},
		Spec: batchv1.JobSpec{
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: pipelineArgs.ConfigurePipelineJobLabels(objHash),
				},
				Spec: v1.PodSpec{
					RestartPolicy:      v1.RestartPolicyOnFailure,
					ServiceAccountName: pipelineArgs.ServiceAccountName(),
					Containers: []v1.Container{
						{
							Name:    "status-writer",
							Image:   os.Getenv("WC_IMG"),
							Command: []string{"sh", "-c", "update-status"},
							Env: []v1.EnvVar{
								{Name: "OBJECT_KIND", Value: strings.ToLower(obj.GetKind())},
								{Name: "OBJECT_GROUP", Value: obj.GroupVersionKind().Group},
								{Name: "OBJECT_NAME", Value: obj.GetName()},
								{Name: "OBJECT_NAMESPACE", Value: pipelineArgs.Namespace()},
							},
							VolumeMounts: []v1.VolumeMount{{
								MountPath: "/work-creator-files/metadata",
								Name:      "shared-metadata",
							}},
						},
					},
					ImagePullSecrets: imagePullSecrets,
					InitContainers:   initContainers,
					Volumes:          volumes,
				},
			},
		},
	}

	if err := controllerutil.SetControllerReference(obj, job, scheme.Scheme); err != nil {
		logger.Error(err, "Error setting ownership")
		return nil, err
	}
	return job, nil
}

func generateConfigurePipelineContainersAndVolumes(obj *unstructured.Unstructured, pipeline v1alpha1.Pipeline, promiseName string, promiseWorkflow bool, logger logr.Logger) ([]v1.Container, []v1.Volume) {
	workflowType := v1alpha1.WorkflowTypeResource
	if promiseWorkflow {
		workflowType = v1alpha1.WorkflowTypePromise
	}

	kratixEnvVars := []v1.EnvVar{
		{
			Name:  kratixActionEnvVar,
			Value: string(v1alpha1.WorkflowActionConfigure),
		},
		{
			Name:  kratixTypeEnvVar,
			Value: string(workflowType),
		},
		{
			Name:  kratixPromiseEnvVar,
			Value: promiseName,
		},
	}

	containers, volumes := generateContainersAndVolumes(obj, workflowType, pipeline, kratixEnvVars)

	workCreatorCommand := fmt.Sprintf("./work-creator -input-directory /work-creator-files -promise-name %s -pipeline-name %s", promiseName, pipeline.Name)
	if promiseWorkflow {
		workCreatorCommand += fmt.Sprintf(" -namespace %s -workflow-type %s", v1alpha1.SystemNamespace, v1alpha1.WorkflowTypePromise)
	} else {
		workCreatorCommand += fmt.Sprintf(" -namespace %s -resource-name %s -workflow-type %s", obj.GetNamespace(), obj.GetName(), v1alpha1.WorkflowTypeResource)
	}

	writer := v1.Container{
		Name:    "work-writer",
		Image:   os.Getenv("WC_IMG"),
		Command: []string{"sh", "-c", workCreatorCommand},
		VolumeMounts: []v1.VolumeMount{
			{
				MountPath: "/work-creator-files/input",
				Name:      "shared-output",
			},
			{
				MountPath: "/work-creator-files/metadata",
				Name:      "shared-metadata",
			},
			{
				MountPath: "/work-creator-files/kratix-system",
				Name:      "promise-scheduling", // this volumemount is a configmap
			},
		},
	}

	containers = append(containers, writer)

	return containers, volumes
}

func metadataAndSchedulingVolumes(configMapName string) []v1.Volume {
	return []v1.Volume{
		{
			Name: "metadata", VolumeSource: v1.VolumeSource{EmptyDir: &v1.EmptyDirVolumeSource{}},
		},
		{
			Name: "promise-scheduling",
			VolumeSource: v1.VolumeSource{
				ConfigMap: &v1.ConfigMapVolumeSource{
					LocalObjectReference: v1.LocalObjectReference{
						Name: configMapName,
					},
					Items: []v1.KeyToPath{{
						Key:  "destinationSelectors",
						Path: "promise-scheduling",
					}},
				},
			},
		},
	}
}
