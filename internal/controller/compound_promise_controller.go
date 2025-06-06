/*
Copyright 2021 Syntasso.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"

	"github.com/go-logr/logr"
	k8sErrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/syntasso/kratix/api/v1alpha1"
	"github.com/syntasso/kratix/lib/compression"
	apiextensionsv1cs "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/serializer/yaml"
)

// CompoundPromiseReconciler reconciles a CompoundPromise object
type CompoundPromiseReconciler struct {
	Client client.Client
	Log    logr.Logger

	ApiextensionsClient apiextensionsv1cs.CustomResourceDefinitionsGetter
	VersionCache        map[string]string
}

var decUnstructured = yaml.NewDecodingSerializer(unstructured.UnstructuredJSONScheme)

//+kubebuilder:rbac:groups="*",resources="*",verbs=get;list;watch;create;update;patch;delete

func (r *CompoundPromiseReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := r.Log.WithValues("compoundpromise", req.NamespacedName)

	workPlacement := &v1alpha1.WorkPlacement{}
	err := r.Client.Get(context.Background(), req.NamespacedName, workPlacement)
	if err != nil {
		if k8sErrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Error getting CompoundPromise", "workPlacement", req.Name)
		return defaultRequeue, nil
	}

	if workPlacement.Spec.TargetDestinationName != "platform" {
		return ctrl.Result{}, nil
	}

	unstructuredResources := []*unstructured.Unstructured{}
	for _, workload := range workPlacement.Spec.Workloads {
		// Step 1: Decode base64-encoded YAML
		decodedContent, err := compression.DecompressContent([]byte(workload.Content))
		if err != nil {
			logger.Error(err, "Error decoding base64 content")
			return ctrl.Result{}, err
		}

		// Step 2: Decode YAML into unstructured.Unstructured
		obj := &unstructured.Unstructured{}
		_, _, err = decUnstructured.Decode(decodedContent, nil, obj)
		if err != nil {
			logger.Error(err, "Error decoding YAML to unstructured")
			return ctrl.Result{}, err
		}

		unstructuredResources = append(unstructuredResources, obj)
	}

	for _, unstructuredResource := range unstructuredResources {
		//get the latest version of the resource
		logger := logger.WithValues("usresource", unstructuredResource.GetName(), "uskind", unstructuredResource.GetKind())
		us := &unstructured.Unstructured{}
		us.SetGroupVersionKind(unstructuredResource.GroupVersionKind())
		err := r.Client.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: unstructuredResource.GetName()}, us)
		if err != nil {
			return ctrl.Result{}, err
		}

		statusMap, ok := us.Object["status"].(map[string]any)
		if !ok {
			logger.Info("Resource does not have a valid status field", "resource", unstructuredResource.GetName())
			return defaultRequeue, nil
		}

		conditions, ok := statusMap["conditions"].([]any)
		if !ok {
			logger.Info("Resource status does not have valid conditions", "resource", unstructuredResource.GetName(), "conditions", statusMap["conditions"])
			return defaultRequeue, nil
		}

		for _, cond := range conditions {
			condMap, ok := cond.(map[string]any)
			if !ok {
				logger.Info("Skipping invalid condition entry", "resource", unstructuredResource.GetName(), "raw", cond)
				continue
			}

			condType, _ := condMap["type"].(string)
			condStatus, _ := condMap["status"].(string)

			if condType == "ConfigureWorkflowCompleted" && condStatus == "True" {
				logger.Info("Resource is ready", "resource", unstructuredResource.GetName())
				break
			}

			if condType == "ConfigureWorkflowCompleted" && condStatus == "False" {
				logger.Info("Resource is not ready yet", "resource", unstructuredResource.GetName(), "condition", condMap)
				return defaultRequeue, nil
			}
		}
	}

	logger.Info("All resources are condition ready", "workPlacement", workPlacement.Name)
	lo := metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(labels.Set(map[string]string{"kratix.io/promise-name": workPlacement.Spec.PromiseName})).String(),
	}
	customResourceDefinitions, err := r.ApiextensionsClient.CustomResourceDefinitions().List(ctx, lo)
	if err != nil {
		logger.Error(err, "Error listing CustomResourceDefinitions for Promise", "promiseName", workPlacement.Spec.PromiseName)
		return ctrl.Result{}, err
	}

	if len(customResourceDefinitions.Items) == 0 {
		logger.Info("No CustomResourceDefinitions found for Promise", "promiseName", workPlacement.Spec.PromiseName)
		return ctrl.Result{}, nil
	}

	crd := &customResourceDefinitions.Items[0]

	pipelineTriggers := &v1alpha1.PipelineTriggerList{}
	err = r.Client.List(ctx, pipelineTriggers, &client.ListOptions{LabelSelector: labels.SelectorFromSet(labels.Set(map[string]string{
		"kratix.io/resource-name": workPlacement.Spec.ResourceName,
		"kratix.io/pipeline-name": workPlacement.Labels["kratix.io/pipeline-name"],
		"kratix.io/group":         crd.Spec.Group,
		"kratix.io/version":       crd.Spec.Versions[0].Name,
		"kratix.io/kind":          crd.Spec.Names.Kind,
	}))})
	if err != nil {
		logger.Error(err, "Error listing PipelineTriggers for WorkPlacement", "workPlacement", workPlacement.Name)
		return ctrl.Result{}, err
	}

	for _, trigger := range pipelineTriggers.Items {
		if trigger.Spec.Triggered {
			logger.Info("PipelineTrigger already triggered", "name", trigger.Name, "namespace", trigger.Namespace)
			continue
		}

		trigger.Spec.Triggered = true
		if err := r.Client.Update(context.Background(), &trigger); err != nil {
			logger.Error(err, "Error updating PipelineTrigger", "name", trigger.Name, "namespace", trigger.Namespace)
			return ctrl.Result{}, err
		}
		logger.Info("Updated PipelineTrigger resource", "name", trigger.Name, "namespace", trigger.Namespace)
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *CompoundPromiseReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.WorkPlacement{}).
		Complete(r)
}
