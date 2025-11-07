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

	"github.com/syntasso/kratix/api/v1alpha1"
	"github.com/syntasso/kratix/lib/workflow"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// PipelineReconciler reconciles a Pipeline object
type PipelineReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	EventRecorder record.EventRecorder
}

// +kubebuilder:rbac:groups=platform.kratix.io.kratix.io,resources=pipelines,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=platform.kratix.io.kratix.io,resources=pipelines/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=platform.kratix.io.kratix.io,resources=pipelines/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Pipeline object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.22.1/pkg/reconcile
func (r *PipelineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = logf.FromContext(ctx)

	pipeline := v1alpha1.Pipeline{}
	err := r.Get(ctx, req.NamespacedName, &pipeline)
	if err != nil {
		if client.IgnoreNotFound(err) == nil {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	logger := logf.FromContext(ctx).WithValues("pipeline", req.NamespacedName)

	ownerRef := pipeline.Spec.OwnerRef

	unstructuredRes := &unstructured.Unstructured{}
	resources := []v1alpha1.PipelineJobResources{}
	pipelineType := ""

	promiseName := ownerRef.PromiseName
	if ownerRef.PromiseName == "" {
		promiseName = ownerRef.Name
	}

	promise := v1alpha1.Promise{}
	err = r.Get(ctx, client.ObjectKey{Name: promiseName}, &promise)
	if err != nil {
		logger.Error(err, "failed to get promise resource for promise pipeline")
		return ctrl.Result{}, err
	}

	if ownerRef.Kind == "Promise" {
		pipelineType = "promise"
		resources, err = promise.GeneratePromisePipelines(v1alpha1.WorkflowActionConfigure, logger)
		if err != nil {
			return ctrl.Result{}, err
		}
		unstructuredRes, err = promise.ToUnstructured()
		if err != nil {
			return ctrl.Result{}, err
		}
	} else {
		gvk := schema.GroupVersionKind{
			Group:   ownerRef.Group,
			Kind:    ownerRef.Kind,
			Version: ownerRef.Version,
		}
		unstructuredRes.SetGroupVersionKind(gvk)

		if err := r.Get(ctx, req.NamespacedName, unstructuredRes); err != nil {
			logger.Error(err, "failed to get resource for resource pipeline")
			return ctrl.Result{}, err
		}

		pipelineType = "resource"

		resources, err = promise.GenerateResourcePipelines(v1alpha1.WorkflowActionConfigure, unstructuredRes, logger)
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	jobOpts := workflow.NewOpts(ctx, r.Client, r.EventRecorder, logger, unstructuredRes, resources, pipelineType, 1, req.Namespace)

	abort, err := reconcileConfigure(jobOpts)
	if err != nil {
		return ctrl.Result{}, err
	}

	if abort {
		return ctrl.Result{}, nil
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *PipelineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		// Uncomment the following line adding a pointer to an instance of the controlled resource as an argument
		// For().
		Named("pipeline").
		Complete(r)
}
