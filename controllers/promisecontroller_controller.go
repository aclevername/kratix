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
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controllerutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/go-logr/logr"
	"github.com/syntasso/kratix/api/v1alpha1"
	platformv1alpha1 "github.com/syntasso/kratix/api/v1alpha1"
)

var finalizer = FinalizerPrefix + "promisecontroller"

// PromiseControllerReconciler reconciles a PromiseController object
type PromiseControllerReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=platform.kratix.io,resources=promisecontrollers,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=platform.kratix.io,resources=promisecontrollers/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=platform.kratix.io,resources=promisecontrollers/finalizers,verbs=update

//+kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="apps",resources=deployments,verbs=create;list;watch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the PromiseController object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.14.1/pkg/reconcile
func (r *PromiseControllerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {

	promiseController := &v1alpha1.PromiseController{}
	err := r.Client.Get(ctx, req.NamespacedName, promiseController)
	if err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		r.Log.Error(err, "Failed getting Promise", "namespacedName", req.NamespacedName)
		return defaultRequeue, nil
	}

	logger := r.Log.WithValues("identifier", promiseController.Name)

	if !promiseController.DeletionTimestamp.IsZero() {
		err := r.deleteDynamicControllerResources(ctx, promiseController, logger)
		if err != nil {
			return defaultRequeue, nil
		}
		return defaultRequeue, nil
	}

	finalizers := []string{finalizer}
	if FinalizersAreMissing(promiseController, finalizers) {
		return AddFinalizers(ctx, r.Client, promiseController, finalizers, logger)
	}

	return ctrl.Result{}, r.createResourcesForDynamicController(ctx, *promiseController, logger)
}

func (r *PromiseControllerReconciler) createResourcesForDynamicController(ctx context.Context, promiseController v1alpha1.PromiseController, logger logr.Logger) error {
	commonLabels := map[string]string{
		"kratix-promise-id": promiseController.Name,
	}

	// CONTROLLER RBAC
	cr := rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name:   promiseController.Name,
			Labels: commonLabels,
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{promiseController.Spec.Group},
				Resources: []string{promiseController.Spec.Plural},
				Verbs:     []string{"get", "list", "update", "create", "patch", "delete", "watch"},
			},
			{
				APIGroups: []string{promiseController.Spec.Group},
				Resources: []string{promiseController.Spec.Plural + "/finalizers"},
				Verbs:     []string{"update"},
			},
			{
				APIGroups: []string{promiseController.Spec.Group},
				Resources: []string{promiseController.Spec.Plural + "/status"},
				Verbs:     []string{"get", "update", "patch"},
			},
		},
	}
	err := r.Client.Create(ctx, &cr)
	if err != nil {
		if errors.IsAlreadyExists(err) {
			// TODO: Test for existence and handle updates of all Promise resources gracefully.
			//
			// WARNING: This return means we will stop reconcilation here if the resource already exists!
			//       		All code below this we only attempt once (on first successful creation).
			return err
		}
		logger.Error(err, "Error creating ClusterRole")
	}

	crb := rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:   promiseController.Name,
			Labels: commonLabels,
		},
		RoleRef: rbacv1.RoleRef{
			Kind:     "ClusterRole",
			APIGroup: "rbac.authorization.k8s.io",
			Name:     cr.Name,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Namespace: "kratix-platform-system",
				Name:      "kratix-platform-controller-manager",
			},
		},
	}

	err = r.Client.Create(ctx, &crb)
	if err != nil {
		logger.Error(err, "Error creating ClusterRoleBinding")
	}
	// END CONTROLLER RBAC

	// PIPELINE RBAC
	cr = rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name:   promiseController.Name + "-promise-pipeline",
			Labels: commonLabels,
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{promiseController.Spec.Group},
				Resources: []string{promiseController.Spec.Plural, promiseController.Spec.Plural + "/status"},
				Verbs:     []string{"get", "list", "update", "create", "patch"},
			},
			{
				APIGroups: []string{"platform.kratix.io"},
				Resources: []string{"works"},
				Verbs:     []string{"get", "update", "create", "patch"},
			},
		},
	}
	err = r.Client.Create(ctx, &cr)
	if err != nil {
		logger.Error(err, "Error creating ClusterRole")
	}

	logger.Info("Creating Service Account")
	sa := v1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      promiseController.Name + "-promise-pipeline",
			Namespace: "default",
			Labels:    commonLabels,
		},
	}

	crb = rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:   promiseController.Name + "-promise-pipeline",
			Labels: commonLabels,
		},
		RoleRef: rbacv1.RoleRef{
			Kind:     "ClusterRole",
			APIGroup: "rbac.authorization.k8s.io",
			Name:     cr.GetName(),
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Namespace: "default",
				Name:      sa.GetName(),
			},
		},
	}

	logger.Info("Creating CRB")
	err = r.Client.Create(ctx, &crb)
	if err != nil {
		logger.Error(err, "Error creating ClusterRoleBinding")
	}

	err = r.Client.Create(ctx, &sa)
	if err != nil {
		logger.Error(err, "Error creating ServiceAccount for Promise")
	} else {
		logger.Info("Created ServiceAccount for Promise")
	}

	configMapName := "cluster-selectors-" + promiseController.Name
	configMapNamespace := "default"

	configMap := v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configMapName,
			Namespace: configMapNamespace,
			Labels:    commonLabels,
		},
		Data: map[string]string{
			"selectors": labels.FormatLabels(promiseController.Spec.ClusterSelector),
		},
	}

	logger.Info("Creating selector configmap")
	err = r.Client.Create(ctx, &configMap)
	if err != nil {
		logger.Error(err, "Error creating config map", "configMap", configMap.Name)
	}

	controllerName := promiseController.Name
	controllerNamespace := "kratix-platform-system"
	controllerConfigMap := v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      controllerName,
			Namespace: controllerNamespace,
			Labels:    commonLabels,
		},
		Data: map[string]string{
			"selectors":  labels.FormatLabels(promiseController.Spec.ClusterSelector),
			"identifier": promiseController.Name,
			"pipeline":   strings.Join(promiseController.Spec.Pipelines, ","),
			"uid":        promiseController.Spec.UID,
			"group":      promiseController.Spec.Group,
			"version":    promiseController.Spec.Version,
			"kind":       promiseController.Spec.Kind,
		},
	}

	logger.Info("Creating controller configmap")
	err = r.Client.Create(ctx, &controllerConfigMap)
	if err != nil {
		logger.Error(err, "Error creating config map", "configMap", controllerConfigMap.Name)
	}

	replicas := int32(1)
	pod := appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      controllerName,
			Namespace: controllerNamespace,
			Labels:    commonLabels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: commonLabels},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Name:      controllerName,
					Namespace: controllerNamespace,
					Labels:    commonLabels,
				},
				Spec: v1.PodSpec{
					RestartPolicy:      v1.RestartPolicyAlways,
					ServiceAccountName: "kratix-platform-controller-manager",
					Volumes: []v1.Volume{
						{
							Name: "config",
							VolumeSource: v1.VolumeSource{
								ConfigMap: &v1.ConfigMapVolumeSource{
									LocalObjectReference: v1.LocalObjectReference{
										Name: controllerName,
									},
								},
							},
						},
					},
					Containers: []v1.Container{
						{
							Name:    "controller",
							Image:   "aclevername/dynamic-controller:v0.1.0",
							Command: []string{"/manager"},
							Env: []v1.EnvVar{
								{Name: "WC_IMG", Value: os.Getenv("WC_IMG")},
							},
							VolumeMounts: []v1.VolumeMount{
								{
									MountPath: "/config",
									Name:      "config",
								},
							},
							LivenessProbe: &v1.Probe{
								ProbeHandler: v1.ProbeHandler{
									HTTPGet: &v1.HTTPGetAction{
										Path: "/readyz",
										Port: intstr.IntOrString{IntVal: int32(8081)},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	logger.Info("Creating controller deployment")
	err = r.Client.Create(ctx, &pod)
	if err != nil {
		logger.Error(err, "Error creating pod", "pod", pod.Name)
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *PromiseControllerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&platformv1alpha1.PromiseController{}).
		Complete(r)
}

func (r *PromiseControllerReconciler) deleteDynamicControllerResources(ctx context.Context, promiseController *v1alpha1.PromiseController, logger logr.Logger) error {
	logger.Info("resource", "resource", promiseController)
	var controllerResourcesToDelete []schema.GroupVersionKind
	controllerResourcesToDelete = append(controllerResourcesToDelete, schema.GroupVersionKind{
		Group:   appsv1.SchemeGroupVersion.Group,
		Version: appsv1.SchemeGroupVersion.Version,
		Kind:    "Deployment",
	})

	controllerResourcesToDelete = append(controllerResourcesToDelete, schema.GroupVersionKind{
		Group:   v1.SchemeGroupVersion.Group,
		Version: v1.SchemeGroupVersion.Version,
		Kind:    "ConfigMap",
	})

	labels := map[string]string{
		"kratix-promise-id": promiseController.Name,
	}
	for _, gvk := range controllerResourcesToDelete {
		resourcesRemaining, err := DeleteAllResourcesWithKindMatchingLabel(ctx, r.Client, gvk, labels, "kratix-platform-system", logger)
		if err != nil {
			return err
		}

		if resourcesRemaining {
			return nil
		}
	}

	logger.Info("removing finalizers")
	controllerutil.RemoveFinalizer(promiseController, finalizer)
	return r.Client.Update(ctx, promiseController)
}
