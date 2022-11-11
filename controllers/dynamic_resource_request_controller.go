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

package controllers

import (
	"context"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"fmt"
	"strings"

	"github.com/go-logr/logr"
	"github.com/syntasso/kratix/api/v1alpha1"

	tekton "github.com/tektoncd/pipeline/pkg/apis/pipeline/v1beta1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/util/uuid"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

const (
	workFinalizer     = finalizerPrefix + "work-cleanup"
	pipelineFinalizer = finalizerPrefix + "pipeline-cleanup"
)

var rrFinalizers = []string{workFinalizer, pipelineFinalizer}

type dynamicResourceRequestController struct {
	client                 client.Client
	gvk                    *schema.GroupVersionKind
	scheme                 *runtime.Scheme
	promiseIdentifier      string
	promiseClusterSelector labels.Set
	xaasRequestPipeline    []string
	log                    logr.Logger
	finalizers             []string
}

//+kubebuilder:rbac:groups=tekton.dev,resources=taskruns,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=create

func (r *dynamicResourceRequestController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := r.log.WithValues(r.promiseIdentifier, req.NamespacedName)

	resourceRequestIdentifier := fmt.Sprintf("%s-%s-%s", r.promiseIdentifier, req.Namespace, req.Name)

	unstructuredCRD := &unstructured.Unstructured{}
	unstructuredCRD.SetGroupVersionKind(*r.gvk)

	err := r.client.Get(ctx, req.NamespacedName, unstructuredCRD)
	if err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed getting Promise CRD")
		return ctrl.Result{}, nil
	}

	if !unstructuredCRD.GetDeletionTimestamp().IsZero() {
		return r.deleteResources(ctx, unstructuredCRD, resourceRequestIdentifier, logger)
	}

	// Reconcile necessary finalizers
	if finalizersAreMissing(unstructuredCRD, []string{workFinalizer, pipelineFinalizer}) {
		return addFinalizers(ctx, r.client, unstructuredCRD, []string{workFinalizer, pipelineFinalizer}, logger)
	}

	if r.pipelineHasExecuted(resourceRequestIdentifier) {
		logger.Info("Cannot execute update on pre-existing pipeline for Promise resource request " + resourceRequestIdentifier)
		return ctrl.Result{}, nil
	}

	taskRun := tekton.TaskRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "request-pipeline-" + r.promiseIdentifier + "-" + getShortUuid(),
			Namespace: "default",
			Labels: map[string]string{
				"kratix-promise-id":                  r.promiseIdentifier,
				"kratix-promise-resource-request-id": resourceRequestIdentifier,
			},
		},
		Spec: tekton.TaskRunSpec{
			ServiceAccountName: r.promiseIdentifier + "-sa",
			Params: []tekton.Param{
				{
					Name: "resourceKind",
					Value: tekton.ParamValue{
						StringVal: strings.ToLower(r.gvk.Kind) + "." + r.gvk.Group,
						Type:      tekton.ParamTypeString,
					},
				},
				{
					Name: "resourceName",
					Value: tekton.ParamValue{
						StringVal: req.Name,
						Type:      tekton.ParamTypeString,
					},
				},
				{
					Name: "resourceNamespace",
					Value: tekton.ParamValue{
						StringVal: req.Namespace,
						Type:      tekton.ParamTypeString,
					},
				},
				{
					Name: "promiseIdentifier",
					Value: tekton.ParamValue{
						StringVal: r.promiseIdentifier,
						Type:      tekton.ParamTypeString,
					},
				},
				{
					Name: "pipelineImage",
					Value: tekton.ParamValue{
						StringVal: r.xaasRequestPipeline[0],
						Type:      tekton.ParamTypeString,
					},
				},
			},
			TaskRef: &tekton.TaskRef{
				Name: "kratix-pipeline",
			},
			Workspaces: []tekton.WorkspaceBinding{
				{
					Name:     "metadata",
					EmptyDir: &v1.EmptyDirVolumeSource{},
				},
				{
					Name:     "input",
					EmptyDir: &v1.EmptyDirVolumeSource{},
				},
				{
					Name:     "output",
					EmptyDir: &v1.EmptyDirVolumeSource{},
				},
				{
					Name:     "work-creator-files",
					EmptyDir: &v1.EmptyDirVolumeSource{},
				},
			},
		},
	}
	// ---
	// apiVersion: tekton.dev/v1beta1
	// kind: TaskRun
	// metadata:
	//   name: run-request-pipeline
	// spec:
	//   serviceAccountName: jenkins-promise-default-sa
	//   params:
	//     Name: resourceKind
	//       Value: "jenkins.example.promise.syntasso.io"
	//     Name: resourceName
	//       value: "example"
	//     - name: resourceNamespace
	//       value: "default"
	//     - name: promiseIdentifier
	//       value: "jenkins-promise-default"
	//     - name: pipelineImage
	//       value: "syntasso/jenkins-request-pipeline"
	//   taskRef:
	//     name: kratix-pipeline
	//   workspaces:
	//     - name: metadata
	//       emptyDir: {}
	//     - name: input
	//       emptyDir: {}
	//     - name: output
	//       emptydir: {}
	//     - name: work-creator-files
	//       emptydir: {}

	logger.Info("Creating Pipeline for Promise resource request: " + resourceRequestIdentifier + ". The pipeline will now execute...")
	err = r.client.Create(ctx, &taskRun)
	if err != nil {
		logger.Error(err, "Error creating task")
		y, _ := yaml.Marshal(&taskRun)
		logger.Error(err, string(y))
	}

	return ctrl.Result{}, nil
}

func (r *dynamicResourceRequestController) pipelineHasExecuted(resourceRequestIdentifier string) bool {
	isPromise, _ := labels.NewRequirement("kratix-promise-resource-request-id", selection.Equals, []string{resourceRequestIdentifier})
	selector := labels.NewSelector().
		Add(*isPromise)

	listOps := &client.ListOptions{
		Namespace:     "default",
		LabelSelector: selector,
	}

	ol := &tekton.TaskRunList{}
	err := r.client.List(context.Background(), ol, listOps)
	if err != nil {
		fmt.Println(err.Error())
		return false
	}
	return len(ol.Items) > 0
}

func (r *dynamicResourceRequestController) deleteResources(ctx context.Context, resourceRequest *unstructured.Unstructured, resourceRequestIdentifier string, logger logr.Logger) (ctrl.Result, error) {
	if finalizersAreDeleted(resourceRequest, rrFinalizers) {
		return ctrl.Result{}, nil
	}

	if controllerutil.ContainsFinalizer(resourceRequest, workFinalizer) {
		err := r.deleteWork(ctx, resourceRequest, resourceRequestIdentifier, workFinalizer, logger)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, err
	}

	if controllerutil.ContainsFinalizer(resourceRequest, pipelineFinalizer) {
		err := r.deletePipeline(ctx, resourceRequest, resourceRequestIdentifier, pipelineFinalizer, logger)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, err
	}

	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

func (r *dynamicResourceRequestController) deleteWork(ctx context.Context, resourceRequest *unstructured.Unstructured, workName string, finalizer string, logger logr.Logger) error {
	work := &v1alpha1.Work{}
	err := r.client.Get(ctx, types.NamespacedName{
		Namespace: "default",
		Name:      workName,
	}, work)
	if err != nil {
		if errors.IsNotFound(err) {
			// only remove finalizer at this point because deletion success is guaranteed
			controllerutil.RemoveFinalizer(resourceRequest, finalizer)
			if err := r.client.Update(ctx, resourceRequest); err != nil {
				return err
			}
			return nil
		}

		logger.Error(err, "Error locating Work, will try again in 5 seconds", "workName", workName)
		return err
	}

	err = r.client.Delete(ctx, work)
	if err != nil {
		if errors.IsNotFound(err) {
			// only remove finalizer at this point because deletion success is guaranteed
			controllerutil.RemoveFinalizer(resourceRequest, finalizer)
			if err := r.client.Update(ctx, resourceRequest); err != nil {
				return err
			}
			return nil
		}

		logger.Error(err, "Error deleting Work %s, will try again in 5 seconds", "workName", workName)
		return err
	}

	return nil
}

func (r *dynamicResourceRequestController) deletePipeline(ctx context.Context, resourceRequest *unstructured.Unstructured, resourceRequestIdentifier, finalizer string, logger logr.Logger) error {
	pods := &v1.PodList{}
	podLabels := map[string]string{
		"kratix-promise-id":                  r.promiseIdentifier,
		"kratix-promise-resource-request-id": resourceRequestIdentifier,
	}
	listOptions := client.ListOptions{LabelSelector: labels.SelectorFromSet(podLabels)}

	err := r.client.List(ctx, pods, &listOptions)
	if err != nil {
		return err
	}

	if len(pods.Items) == 0 {
		controllerutil.RemoveFinalizer(resourceRequest, finalizer)
		if err := r.client.Update(ctx, resourceRequest); err != nil {
			return err
		}
		return nil
	}

	for _, pod := range pods.Items {
		err = r.client.Delete(ctx, &pod)
		if err != nil && !errors.IsNotFound(err) {
			logger.Error(err, "Error deleting Pod, will try again in 5 seconds", "pod", pod.GetName())
			return err
		}
	}

	return nil

}

func getShortUuid() string {
	envUuid, present := os.LookupEnv("TEST_PROMISE_CONTROLLER_POD_IDENTIFIER_UUID")
	if present {
		return envUuid
	} else {
		return string(uuid.NewUUID()[0:5])
	}
}
