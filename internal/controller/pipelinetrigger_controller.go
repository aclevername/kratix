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

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	uuid "github.com/google/uuid"
	platformkratixiov1alpha1 "github.com/syntasso/kratix/api/v1alpha1"
)

// PipelineTriggerReconciler reconciles a PipelineTrigger object
type PipelineTriggerReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=platform.kratix.io,resources=pipelinetriggers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=platform.kratix.io,resources=pipelinetriggers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=platform.kratix.io,resources=pipelinetriggers/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the PipelineTrigger object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.20.4/pkg/reconcile
func (r *PipelineTriggerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logf.FromContext(ctx).WithValues("PipelineTrigger", req.NamespacedName)

	pipelineTrigger := &platformkratixiov1alpha1.PipelineTrigger{}
	if err := r.Get(ctx, req.NamespacedName, pipelineTrigger); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	logger.Info("Reconciling PipelineTrigger")
	if pipelineTrigger.Spec.Triggered {
		logger.Info("PipelineTrigger is triggered, labeling request")
		unstructreObj := &unstructured.Unstructured{}
		unstructreObj.SetName(pipelineTrigger.Spec.ResourceName)
		unstructreObj.SetNamespace(pipelineTrigger.Spec.ResourceNamespace)
		unstructreObj.SetAPIVersion(pipelineTrigger.Spec.APIVersion)
		unstructreObj.SetKind(pipelineTrigger.Spec.Kind)
		namedNamespace := client.ObjectKey{
			Name:      pipelineTrigger.Spec.ResourceName,
			Namespace: pipelineTrigger.Spec.ResourceNamespace,
		}
		if err := r.Get(ctx, namedNamespace, unstructreObj); err != nil {
			logger.Error(err, "Unable to get resource")
			return ctrl.Result{}, err
		}
		newLabels := unstructreObj.GetLabels()
		if newLabels == nil {
			newLabels = make(map[string]string)
		}
		//generate uuid
		uuid := uuid.New().String()
		newLabels["kratix.io/trigger-reconcile-loop"] = uuid[0:8]
		unstructreObj.SetLabels(newLabels)
		if err := r.Update(ctx, unstructreObj); err != nil {
			logger.Error(err, "Unable to update resource")
			return ctrl.Result{}, err
		}
		logger.Info("Resource labeled successfully")

		logger.Info("Deleting PipelineTrigger")
		//refetch first
		if err := r.Get(ctx, req.NamespacedName, pipelineTrigger); err != nil {
			if client.IgnoreNotFound(err) != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
		if err := r.Delete(ctx, pipelineTrigger); err != nil {
			logger.Error(err, "Unable to delete PipelineTrigger")
			return ctrl.Result{}, err
		}
		logger.Info("PipelineTrigger deleted successfully")
		return ctrl.Result{}, nil
	}

	logger.Info("PipelineTrigger is not triggered")
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *PipelineTriggerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&platformkratixiov1alpha1.PipelineTrigger{}).
		Named("pipelinetrigger").
		Complete(r)
}
