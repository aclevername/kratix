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
	"encoding/json"
	"os"
	"strings"
	"time"

	controllerutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/go-logr/logr"
	"github.com/syntasso/kratix/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// PromiseReconciler reconciles a Promise object
type PromiseReconciler struct {
	Client              client.Client
	ApiextensionsClient *clientset.Clientset
	Log                 logr.Logger
	Manager             ctrl.Manager
	DynamicControllers  map[string]*bool
}

const (
	FinalizerPrefix                                    = "kratix.io/"
	clusterSelectorsConfigMapCleanupFinalizer          = FinalizerPrefix + "cluster-selectors-config-map-cleanup"
	resourceRequestCleanupFinalizer                    = FinalizerPrefix + "resource-request-cleanup"
	dynamicControllerDependantResourcesCleaupFinalizer = FinalizerPrefix + "dynamic-controller-dependant-resources-cleanup"
	crdCleanupFinalizer                                = FinalizerPrefix + "crd-cleanup"
	workerClusterResourcesCleanupFinalizer             = FinalizerPrefix + "worker-cluster-resources-cleanup"
)

var (
	promiseFinalizers = []string{
		clusterSelectorsConfigMapCleanupFinalizer,
		resourceRequestCleanupFinalizer,
		dynamicControllerDependantResourcesCleaupFinalizer,
		crdCleanupFinalizer,
		workerClusterResourcesCleanupFinalizer,
	}

	// fastRequeue can be used whenever we want to quickly requeue, and we don't expect
	// an error to occur. Example: we delete a resource, we then requeue
	// to check it's been deleted. Here we can use a fastRequeue instead of a defaultRequeue
	fastRequeue    = ctrl.Result{RequeueAfter: 1 * time.Second}
	defaultRequeue = ctrl.Result{RequeueAfter: 5 * time.Second}
	slowRequeue    = ctrl.Result{RequeueAfter: 15 * time.Second}
)

//+kubebuilder:rbac:groups=platform.kratix.io,resources=promises,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=platform.kratix.io,resources=promises/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=platform.kratix.io,resources=promises/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=configmaps,verbs=create;list;watch;delete

//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,verbs=create;escalate;bind;list;get;delete;watch
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterrolebindings,verbs=create;list;get;delete;watch
//+kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=create;list;get;watch;delete

//+kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="apps",resources=deployments,verbs=create;list;watch;delete

//TODO remove and use created role
//+kubebuilder:rbac:groups="",resources=pods,verbs=create;list;watch;delete
//+kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=create

func (r *PromiseReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	promise := &v1alpha1.Promise{}
	err := r.Client.Get(ctx, req.NamespacedName, promise)
	if err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		r.Log.Error(err, "Failed getting Promise", "namespacedName", req.NamespacedName)
		return defaultRequeue, nil
	}

	logger := r.Log.WithValues("identifier", promise.GetIdentifier())

	if !promise.DeletionTimestamp.IsZero() {
		return r.deletePromise(ctx, promise, logger)
	}

	finalizers := getDesiredFinalizers(promise)
	if FinalizersAreMissing(promise, finalizers) {
		return AddFinalizers(ctx, r.Client, promise, finalizers, logger)
	}

	if err := r.createWorkResourceForWorkerClusterResources(ctx, promise, logger); err != nil {
		logger.Error(err, "Error creating Works")
		return ctrl.Result{}, err
	}

	if promise.DoesNotContainXAASCrd() {
		logger.Info("Promise only contains WCRs, skipping creation of CRD and dynamic controller")
		return ctrl.Result{}, nil
	}

	rrCRD, rrGVK, err := generateCRDAndGVK(promise, logger)
	if err != nil {
		return ctrl.Result{}, err
	}

	exists := r.ensureCRDExists(ctx, rrCRD, rrGVK, logger)
	if !exists {
		return defaultRequeue, nil
	}

	err = r.createResourcesForDynamicController(ctx, promise, rrCRD, rrGVK, logger)
	if err != nil {
		logger.Error(err, "creating dynamic controller")
	}
	// Since we don't support Updates we cannot tell if createResourcesForDynamicController fails because the resources
	// already exist or for other reasons, so we don't handle the error or requeue
	// TODO add support for updates and gracefully error
	return ctrl.Result{}, nil

}

func getDesiredFinalizers(promise *v1alpha1.Promise) []string {
	if promise.DoesNotContainXAASCrd() {
		return []string{workerClusterResourcesCleanupFinalizer}
	}
	return promiseFinalizers
}

func (r *PromiseReconciler) createResourcesForDynamicController(ctx context.Context, promise *v1alpha1.Promise,
	rrCRD *apiextensionsv1.CustomResourceDefinition, rrGVK schema.GroupVersionKind, logger logr.Logger) error {

	// CONTROLLER RBAC
	cr := rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name:   promise.GetControllerResourceName(),
			Labels: promise.GenerateSharedLabels(),
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{rrGVK.Group},
				Resources: []string{rrCRD.Spec.Names.Plural},
				Verbs:     []string{"get", "list", "update", "create", "patch", "delete", "watch"},
			},
			{
				APIGroups: []string{rrGVK.Group},
				Resources: []string{rrCRD.Spec.Names.Plural + "/finalizers"},
				Verbs:     []string{"update"},
			},
			{
				APIGroups: []string{rrGVK.Group},
				Resources: []string{rrCRD.Spec.Names.Plural + "/status"},
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
			Name:   promise.GetControllerResourceName(),
			Labels: promise.GenerateSharedLabels(),
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
			Name:   promise.GetPipelineResourceName(),
			Labels: promise.GenerateSharedLabels(),
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{rrGVK.Group},
				Resources: []string{rrCRD.Spec.Names.Plural, rrCRD.Spec.Names.Plural + "/status"},
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
			Name:      promise.GetPipelineResourceName(),
			Namespace: "default",
			Labels:    promise.GenerateSharedLabels(),
		},
	}

	crb = rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:   promise.GetPipelineResourceName(),
			Labels: promise.GenerateSharedLabels(),
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

	configMapName := "cluster-selectors-" + promise.GetIdentifier()
	configMapNamespace := "default"

	configMap := v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configMapName,
			Namespace: configMapNamespace,
			Labels:    promise.GenerateSharedLabels(),
		},
		Data: map[string]string{
			"selectors": labels.FormatLabels(promise.Spec.ClusterSelector),
		},
	}

	logger.Info("Creating selector configmap")
	err = r.Client.Create(ctx, &configMap)
	if err != nil {
		logger.Error(err, "Error creating config map", "configMap", configMap.Name)
	}

	controllerName := promise.GetControllerResourceName()
	controllerNamespace := "kratix-platform-system"
	controllerConfigMap := v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      controllerName,
			Namespace: controllerNamespace,
			Labels:    promise.GenerateSharedLabels(),
		},
		Data: map[string]string{
			"selectors":  labels.FormatLabels(promise.Spec.ClusterSelector),
			"identifier": promise.GetIdentifier(),
			"pipeline":   strings.Join(promise.Spec.XaasRequestPipeline, ","),
			"uid":        string(promise.GetUID()),
			"group":      rrGVK.Group,
			"version":    rrGVK.Version,
			"kind":       rrGVK.Kind,
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
			Labels:    promise.GenerateSharedLabels(),
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: promise.GenerateSharedLabels()},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Name:      controllerName,
					Namespace: controllerNamespace,
					Labels:    promise.GenerateSharedLabels(),
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

func (r *PromiseReconciler) ensureCRDExists(ctx context.Context, rrCRD *apiextensionsv1.CustomResourceDefinition,
	rrGVK schema.GroupVersionKind, logger logr.Logger) bool {

	_, err := r.ApiextensionsClient.ApiextensionsV1().
		CustomResourceDefinitions().
		Create(ctx, rrCRD, metav1.CreateOptions{})
	if err != nil {
		if errors.IsAlreadyExists(err) {
			//todo test for existence and handle gracefully.
			logger.Info("CRD already exists", "crdName", rrCRD.Name)
		} else {
			logger.Error(err, "Error creating crd")
		}
	}

	_, err = r.Manager.GetRESTMapper().RESTMapping(rrGVK.GroupKind(), rrGVK.Version)
	return err == nil
}

func (r *PromiseReconciler) deletePromise(ctx context.Context, promise *v1alpha1.Promise, logger logr.Logger) (ctrl.Result, error) {
	if FinalizersAreDeleted(promise, promiseFinalizers) {
		return ctrl.Result{}, nil
	}

	if controllerutil.ContainsFinalizer(promise, resourceRequestCleanupFinalizer) {
		logger.Info("deleting resources associated with finalizer", "finalizer", resourceRequestCleanupFinalizer)
		err := r.deleteResourceRequests(ctx, promise, logger)
		if err != nil {
			return defaultRequeue, err
		}
		return fastRequeue, nil
	}

	if controllerutil.ContainsFinalizer(promise, clusterSelectorsConfigMapCleanupFinalizer) {
		logger.Info("deleting resources associated with finalizer", "finalizer", clusterSelectorsConfigMapCleanupFinalizer)
		err := r.deleteConfigMap(ctx, promise, logger)
		if err != nil {
			return defaultRequeue, err
		}
		return fastRequeue, nil
	}

	if controllerutil.ContainsFinalizer(promise, dynamicControllerDependantResourcesCleaupFinalizer) {
		logger.Info("deleting resources associated with finalizer", "finalizer", dynamicControllerDependantResourcesCleaupFinalizer)
		err := r.deleteDynamicControllerResources(ctx, promise, logger)
		if err != nil {
			return defaultRequeue, err
		}
		return fastRequeue, nil
	}

	if controllerutil.ContainsFinalizer(promise, crdCleanupFinalizer) {
		logger.Info("deleting CRDs associated with finalizer", "finalizer", crdCleanupFinalizer)
		err := r.deleteCRDs(ctx, promise, logger)
		if err != nil {
			return defaultRequeue, err
		}
		return fastRequeue, nil
	}

	if controllerutil.ContainsFinalizer(promise, workerClusterResourcesCleanupFinalizer) {
		logger.Info("deleting Work associated with finalizer", "finalizer", workerClusterResourcesCleanupFinalizer)
		err := r.deleteWork(ctx, promise, logger)
		if err != nil {
			return defaultRequeue, err
		}
		return fastRequeue, nil
	}

	return fastRequeue, nil
}

// crb + cr + sa things we create for the pipeline
// crb + cr attach to our existing service account
func (r *PromiseReconciler) deleteDynamicControllerResources(ctx context.Context, promise *v1alpha1.Promise, logger logr.Logger) error {
	var resourcesToDelete []schema.GroupVersionKind
	resourcesToDelete = append(resourcesToDelete, schema.GroupVersionKind{
		Group:   rbacv1.SchemeGroupVersion.Group,
		Version: rbacv1.SchemeGroupVersion.Version,
		Kind:    "ClusterRoleBinding",
	})

	resourcesToDelete = append(resourcesToDelete, schema.GroupVersionKind{
		Group:   rbacv1.SchemeGroupVersion.Group,
		Version: rbacv1.SchemeGroupVersion.Version,
		Kind:    "ClusterRole",
	})

	resourcesToDelete = append(resourcesToDelete, schema.GroupVersionKind{
		Group:   v1.SchemeGroupVersion.Group,
		Version: v1.SchemeGroupVersion.Version,
		Kind:    "ServiceAccount",
	})

	for _, gvk := range resourcesToDelete {
		resourcesRemaining, err := DeleteAllResourcesWithKindMatchingLabel(ctx, r.Client, gvk, promise.GenerateSharedLabels(), "default", logger)
		if err != nil {
			return err
		}

		if resourcesRemaining {
			return nil
		}
	}

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

	for _, gvk := range controllerResourcesToDelete {
		resourcesRemaining, err := DeleteAllResourcesWithKindMatchingLabel(ctx, r.Client, gvk, promise.GenerateSharedLabels(), "kratix-platform-system", logger)
		if err != nil {
			return err
		}

		if resourcesRemaining {
			return nil
		}
	}

	controllerutil.RemoveFinalizer(promise, dynamicControllerDependantResourcesCleaupFinalizer)
	return r.Client.Update(ctx, promise)
}

func (r *PromiseReconciler) deleteResourceRequests(ctx context.Context, promise *v1alpha1.Promise, logger logr.Logger) error {
	_, rrGVK, err := generateCRDAndGVK(promise, logger)
	if err != nil {
		return err
	}

	// No need to pass labels since all resource requests are of Kind
	resourcesRemaining, err := DeleteAllResourcesWithKindMatchingLabel(ctx, r.Client, rrGVK, nil, "default", logger)
	if err != nil {
		return err
	}

	if !resourcesRemaining {
		controllerutil.RemoveFinalizer(promise, resourceRequestCleanupFinalizer)
		if err := r.Client.Update(ctx, promise); err != nil {
			return err
		}
	}

	return nil
}

func (r *PromiseReconciler) deleteConfigMap(ctx context.Context, promise *v1alpha1.Promise, logger logr.Logger) error {
	gvk := schema.GroupVersionKind{
		Group:   v1.SchemeGroupVersion.Group,
		Version: v1.SchemeGroupVersion.Version,
		Kind:    "ConfigMap",
	}

	resourcesRemaining, err := DeleteAllResourcesWithKindMatchingLabel(ctx, r.Client, gvk, promise.GenerateSharedLabels(), "default", logger)
	if err != nil {
		return err
	}

	if !resourcesRemaining {
		controllerutil.RemoveFinalizer(promise, clusterSelectorsConfigMapCleanupFinalizer)
		return r.Client.Update(ctx, promise)
	}

	return nil
}

func (r *PromiseReconciler) deleteCRDs(ctx context.Context, promise *v1alpha1.Promise, logger logr.Logger) error {
	crdGVK := schema.GroupVersionKind{
		Group:   apiextensionsv1.SchemeGroupVersion.Group,
		Version: apiextensionsv1.SchemeGroupVersion.Version,
		Kind:    "CustomResourceDefinition",
	}

	resourcesRemaining, err := DeleteAllResourcesWithKindMatchingLabel(ctx, r.Client, crdGVK, promise.GenerateSharedLabels(), "default", logger)
	if err != nil {
		return err
	}

	if !resourcesRemaining {
		controllerutil.RemoveFinalizer(promise, crdCleanupFinalizer)
		if err := r.Client.Update(ctx, promise); err != nil {
			return err
		}
	}

	return nil
}

func (r *PromiseReconciler) deleteWork(ctx context.Context, promise *v1alpha1.Promise, logger logr.Logger) error {
	workGVK := schema.GroupVersionKind{
		Group:   v1alpha1.GroupVersion.Group,
		Version: v1alpha1.GroupVersion.Version,
		Kind:    "Work",
	}

	resourcesRemaining, err := DeleteAllResourcesWithKindMatchingLabel(ctx, r.Client, workGVK, promise.GenerateSharedLabels(), "default", logger)
	if err != nil {
		return err
	}

	if !resourcesRemaining {
		controllerutil.RemoveFinalizer(promise, workerClusterResourcesCleanupFinalizer)
		if err := r.Client.Update(ctx, promise); err != nil {
			return err
		}
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *PromiseReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Promise{}).
		Complete(r)
}

func generateCRDAndGVK(promise *v1alpha1.Promise, logger logr.Logger) (*apiextensionsv1.CustomResourceDefinition, schema.GroupVersionKind, error) {
	rrCRD := &apiextensionsv1.CustomResourceDefinition{}
	rrGVK := schema.GroupVersionKind{}

	err := json.Unmarshal(promise.Spec.XaasCrd.Raw, rrCRD)
	if err != nil {
		logger.Error(err, "Failed unmarshalling CRD")
		return rrCRD, rrGVK, err
	}
	rrCRD.Labels = labels.Merge(rrCRD.Labels, promise.GenerateSharedLabels())

	setStatusFieldsOnCRD(rrCRD)

	rrGVK = schema.GroupVersionKind{
		Group:   rrCRD.Spec.Group,
		Version: rrCRD.Spec.Versions[0].Name,
		Kind:    rrCRD.Spec.Names.Kind,
	}

	return rrCRD, rrGVK, nil
}

func setStatusFieldsOnCRD(rrCRD *apiextensionsv1.CustomResourceDefinition) {
	rrCRD.Spec.Versions[0].Subresources = &apiextensionsv1.CustomResourceSubresources{
		Status: &apiextensionsv1.CustomResourceSubresourceStatus{},
	}

	rrCRD.Spec.Versions[0].AdditionalPrinterColumns = []apiextensionsv1.CustomResourceColumnDefinition{
		apiextensionsv1.CustomResourceColumnDefinition{
			Name:     "status",
			Type:     "string",
			JSONPath: ".status.message",
		},
	}

	rrCRD.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"] = apiextensionsv1.JSONSchemaProps{
		Type:                   "object",
		XPreserveUnknownFields: &[]bool{true}[0], // pointer to bool
		Properties: map[string]apiextensionsv1.JSONSchemaProps{
			"message": {
				Type: "string",
			},
			"conditions": {
				Type: "array",
				Items: &apiextensionsv1.JSONSchemaPropsOrArray{
					Schema: &apiextensionsv1.JSONSchemaProps{
						Type: "object",
						Properties: map[string]apiextensionsv1.JSONSchemaProps{
							"lastTransitionTime": {
								Type:   "string",
								Format: "datetime", //RFC3339
							},
							"message": {
								Type: "string",
							},
							"reason": {
								Type: "string",
							},
							"status": {
								Type: "string",
							},
							"type": {
								Type: "string",
							},
						},
					},
				},
			},
		},
	}
}

func (r *PromiseReconciler) createWorkResourceForWorkerClusterResources(ctx context.Context, promise *v1alpha1.Promise, logger logr.Logger) error {
	workToCreate := &v1alpha1.Work{}
	workToCreate.Spec.Replicas = v1alpha1.WorkerResourceReplicas
	workToCreate.Name = promise.GetIdentifier()
	workToCreate.Namespace = "default"
	workToCreate.Labels = promise.GenerateSharedLabels()
	workToCreate.Spec.ClusterSelector = promise.Spec.ClusterSelector
	for _, u := range promise.Spec.WorkerClusterResources {
		workToCreate.Spec.Workload.Manifests = append(workToCreate.Spec.Workload.Manifests, v1alpha1.Manifest{Unstructured: u.Unstructured})
	}

	logger.Info("Creating Work resource for promise")
	err := r.Client.Create(ctx, workToCreate)
	if err != nil {
		if errors.IsAlreadyExists(err) {
			logger.Info("Work already exist", "workName", workToCreate.Name)
			return nil
		}
		return err
	}

	return nil
}
