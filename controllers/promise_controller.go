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
	"fmt"
	"strings"
	"time"

	controllerutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/go-logr/logr"
	"github.com/syntasso/kratix/api/v1alpha1"
	platformv1alpha1 "github.com/syntasso/kratix/api/v1alpha1"
	v1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	yamlsig "sigs.k8s.io/yaml"
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
	kratixPrefix                                       = "kratix.io/"
	resourceRequestCleanupFinalizer                    = kratixPrefix + "resource-request-cleanup"
	dynamicControllerDependantResourcesCleaupFinalizer = kratixPrefix + "dynamic-controller-dependant-resources-cleanup"
	crdCleanupFinalizer                                = kratixPrefix + "api-crd-cleanup"
	dependenciesCleanupFinalizer                       = kratixPrefix + "dependencies-cleanup"
)

var (
	promiseFinalizers = []string{
		resourceRequestCleanupFinalizer,
		dynamicControllerDependantResourcesCleaupFinalizer,
		crdCleanupFinalizer,
		dependenciesCleanupFinalizer,
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
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles,verbs=create;escalate;bind;list;get;delete;watch
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=create;list;get;delete;watch
//+kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=create;list;get;watch;delete

//+kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch;create;update;patch;delete

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
	if finalizersAreMissing(promise, finalizers) {
		return addFinalizers(ctx, r.Client, promise, finalizers, logger)
	}

	if err := r.createWorkResourceForDependencies(ctx, promise, logger); err != nil {
		logger.Error(err, "Error creating Works")
		return ctrl.Result{}, err
	}

	if promise.DoesNotContainAPI() {
		logger.Info("Promise only contains dependencies, skipping creation of API and dynamic controller")
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

	err = r.populateBackstage(ctx, promise, rrCRD, logger)
	if err != nil {
		return ctrl.Result{}, err
	}

	// if the controllers already been started, we will error out and return
	// as it errors when resources for the controller already exist.
	if err := r.createResourcesForDynamicController(ctx, promise, rrCRD, rrGVK, logger); err != nil {
		// Since we don't support Updates we cannot tell if createResourcesForDynamicController fails because the resources
		// already exist or for other reasons, so we don't handle the error or requeue
		// TODO add support for updates and gracefully error
		return ctrl.Result{}, nil
	}

	configurePipelines, deletePipelines, err := r.generatePipelines(promise, logger)
	if err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, r.startDynamicController(promise, rrCRD, rrGVK, configurePipelines, deletePipelines)
}

func getDesiredFinalizers(promise *v1alpha1.Promise) []string {
	if promise.DoesNotContainAPI() {
		return []string{dependenciesCleanupFinalizer}
	}
	return promiseFinalizers
}

func (r *PromiseReconciler) startDynamicController(promise *v1alpha1.Promise, rrCRD *apiextensionsv1.CustomResourceDefinition, rrGVK schema.GroupVersionKind, configurePipelines, deletePipelines []v1alpha1.Pipeline) error {
	//temporary fix until https://github.com/kubernetes-sigs/controller-runtime/issues/1884 is resolved
	//once resolved, delete dynamic controller rather than disable
	enabled := true
	r.DynamicControllers[string(promise.GetUID())] = &enabled

	dynamicResourceRequestController := &dynamicResourceRequestController{
		Client:             r.Manager.GetClient(),
		scheme:             r.Manager.GetScheme(),
		gvk:                &rrGVK,
		crd:                rrCRD,
		promiseIdentifier:  promise.GetIdentifier(),
		promiseScheduling:  promise.Spec.Scheduling,
		configurePipelines: configurePipelines,
		deletePipelines:    deletePipelines,
		log:                r.Log,
		uid:                string(promise.GetUID())[0:5],
		enabled:            &enabled,
	}

	unstructuredCRD := &unstructured.Unstructured{}
	unstructuredCRD.SetGroupVersionKind(rrGVK)

	return ctrl.NewControllerManagedBy(r.Manager).
		For(unstructuredCRD).
		Complete(dynamicResourceRequestController)
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
				Verbs:     []string{rbacv1.VerbAll},
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
				Namespace: KratixSystemNamespace,
				Name:      "kratix-platform-controller-manager",
			},
		},
	}

	err = r.Client.Create(ctx, &crb)
	if err != nil {
		logger.Error(err, "Error creating ClusterRoleBinding")
	}
	// END CONTROLLER RBAC

	return nil
}

type Template struct {
	metav1.TypeMeta `json:",inline,omitempty"`
	Metadata        Metadata     `json:"metadata,omitempty"`
	Spec            TemplateSpec `json:"spec,omitempty"`
}

type Metadata struct {
	metav1.ObjectMeta `json:",inline"`
	Description       string   `json:"description,omitempty"`
	Tags              []string `json:"tags,omitempty"`
	Title             string   `json:"title,omitempty"`
}

type TemplateSpec struct {
	Lifecycle  string      `json:"lifecycle,omitempty"`
	Owner      string      `json:"owner,omitempty"`
	Parameters []Parameter `json:"parameters,omitempty"`
	Steps      []Step      `json:"steps,omitempty"`
	Type       string      `json:"type,omitempty"`
}

type Step struct {
	Action string `json:"action,omitempty"`
	ID     string `json:"id,omitempty"`
	Input  Input  `json:"input,omitempty"`
	Name   string `json:"name,omitempty"`
}

type Input struct {
	Manifest   string `json:"manifest,omitempty"`
	Namespaced bool   `json:"namespaced,omitempty"`
}

type Parameter struct {
	Properties map[string]Properties `json:"properties,omitempty"`
	Required   []string              `json:"required,omitempty"`
	Title      string                `json:"title,omitempty"`
}

type Properties struct {
	Description string `json:"description,omitempty"`
	Title       string `json:"title,omitempty"`
	Type        string `json:"type,omitempty"`
}

type RR struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              map[string]string `json:"spec,omitempty"`
}

func (r *PromiseReconciler) populateBackstage(ctx context.Context, promise *v1alpha1.Promise, rrCRD *apiextensionsv1.CustomResourceDefinition, logger logr.Logger) error {
	template, err := r.generateBackstageTemplate(rrCRD)
	if err != nil {
		return err
	}

	component, err := r.generateBackstageComponent(rrCRD)
	if err != nil {
		return err
	}
	return r.createBackstageWork(promise, rrCRD, component, template)
}

func (r *PromiseReconciler) generateBackstageTemplate(rrCRD *apiextensionsv1.CustomResourceDefinition) (unstructured.Unstructured, error) {
	template, err := r.generateBackstageTemplateWithoutProperties(rrCRD)
	if err != nil {
		return unstructured.Unstructured{}, err
	}

	//Generate the manifest the kubectl plugin will apply based on the paremeters
	rrManifestTemplate := RR{
		TypeMeta: metav1.TypeMeta{
			APIVersion: rrCRD.Spec.Group + "/" + rrCRD.Spec.Versions[0].Name,
			Kind:       rrCRD.Spec.Names.Kind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "${{ parameters.name }}",
			Namespace: "${{ parameters.namespace}}",
			Labels: map[string]string{
				"backstage.io/kubernetes-id": rrCRD.Spec.Names.Kind,
			},
		},
		Spec: map[string]string{},
	}

	//Generate the parameter properties based on the CRD
	props := map[string]Properties{}
	for key, prop := range rrCRD.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties {
		props[key] = Properties{
			Description: prop.Description,
			Title:       "Spec." + strings.Title(key),
			Type:        prop.Type,
		}
		rrManifestTemplate.Spec[key] = fmt.Sprintf("${{ parameters.%s }}", key)
	}

	props["namespace"] = Properties{
		Description: "Namespace for the request in the platform cluster",
		Title:       "Metadata.Namespace",
		Type:        "string",
	}

	props["name"] = Properties{
		Description: "Name for the request in the platform cluster",
		Title:       "Metadata.Name",
		Type:        "string",
	}
	fmt.Println(props)

	sampleRRBytes, err := yamlsig.Marshal(rrManifestTemplate)
	if err != nil {
		return unstructured.Unstructured{}, err

	}

	template.Spec.Steps[0].Input.Manifest = string(sampleRRBytes)
	template.Spec.Parameters = []Parameter{
		{
			Properties: props,
			Required:   append(rrCRD.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Required, "namespace", "name"),
			Title:      strings.Title(rrCRD.Spec.Names.Kind) + " as a Service",
		},
	}

	//Convert to bytes
	templateBytes, err := yamlsig.Marshal(template)
	if err != nil {
		return unstructured.Unstructured{}, err

	}
	fmt.Println(string(templateBytes))

	//Convert back to unstructured.Unstructured
	templateUS := unstructured.Unstructured{}
	err = yamlsig.Unmarshal(templateBytes, &templateUS)
	return templateUS, err
}

func (r *PromiseReconciler) generateBackstageTemplateWithoutProperties(rrCRD *apiextensionsv1.CustomResourceDefinition) (Template, error) {
	//Easier to generate from string than manually fill out go struct
	baseTemplate := []byte(fmt.Sprintf(`---
apiVersion: scaffolder.backstage.io/v1beta3
kind: Template
metadata:
  description: %[2]s as a Service
  name: %[1]s-promise-template
  tags:
  - syntasso
  - kratix
  - experimental
  title: %[2]s
spec:
  lifecycle: experimental
  owner: kratix-platform
  steps:
  - action: kubernetes:apply
    id: k-apply
    input:
      manifest: ""
      namespaced: true
    name: Create a %[1]s
  type: service`, rrCRD.Spec.Names.Kind, strings.Title(rrCRD.Spec.Names.Kind)))

	template := Template{}
	err := yamlsig.Unmarshal(baseTemplate, &template)
	if err != nil {
		return Template{}, fmt.Errorf("failed to unmarshal:"+string(baseTemplate)+": %w", err.Error())
	}
	return template, nil
}

func (r *PromiseReconciler) generateBackstageComponent(rrCRD *apiextensionsv1.CustomResourceDefinition) (unstructured.Unstructured, error) {
	componentBytes := []byte(fmt.Sprintf(`---
apiVersion: backstage.io/v1alpha1
kind: Component
metadata:
  annotations:
    backstage.io/kubernetes-id: %[1]s
  description: Create a %[1]s
  links:
  - icon: help
    title: Support
    url: https://github.com/syntasso/kratix-backstage
  name: %[1]s
  title: %[2]s Promise
spec:
  dependsOn:
  - component:default/kratix
  lifecycle: production
  owner: kratix-platform
  providesApis:
  - %[1]s-promise-api
  type: promise
`, rrCRD.Spec.Names.Kind, strings.Title(rrCRD.Spec.Names.Kind)))

	componentUS := unstructured.Unstructured{}
	err := yamlsig.Unmarshal(componentBytes, &componentUS)
	if err != nil {
		return unstructured.Unstructured{}, err
	}
	return componentUS, err
}

func (r *PromiseReconciler) createBackstageWork(promise *v1alpha1.Promise, rrCRD *apiextensionsv1.CustomResourceDefinition, resources ...unstructured.Unstructured) error {
	work := v1alpha1.Work{}
	work.Name = promise.GetIdentifier() + "-backstage"
	work.Namespace = "kratix-platform-system"
	work.Spec.Replicas = v1alpha1.DependencyReplicas
	work.Spec.Scheduling = v1alpha1.WorkScheduling{
		Promise: []v1alpha1.SchedulingConfig{
			{
				Target: v1alpha1.Target{
					MatchLabels: map[string]string{
						"environment": "backstage",
					},
				},
			},
		},
	}

	manifests := &work.Spec.Workload.Manifests
	for _, resource := range resources {
		manifest := platformv1alpha1.Manifest{
			Unstructured: resource,
		}
		*manifests = append(*manifests, manifest)
	}

	err := r.Client.Create(context.Background(), &work)

	if err != nil {
		if errors.IsAlreadyExists(err) {
			return nil
		}
		return err
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
	if finalizersAreDeleted(promise, promiseFinalizers) {
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

	//temporary fix until https://github.com/kubernetes-sigs/controller-runtime/issues/1884 is resolved
	//once resolved, delete dynamic controller rather than disable
	if enabled, exists := r.DynamicControllers[string(promise.GetUID())]; exists {
		*enabled = false
	}

	if controllerutil.ContainsFinalizer(promise, dynamicControllerDependantResourcesCleaupFinalizer) {
		logger.Info("deleting resources associated with finalizer", "finalizer", dynamicControllerDependantResourcesCleaupFinalizer)
		err := r.deleteDynamicControllerResources(ctx, promise, logger)
		if err != nil {
			return defaultRequeue, err
		}
		return fastRequeue, nil
	}

	if controllerutil.ContainsFinalizer(promise, dependenciesCleanupFinalizer) {
		logger.Info("deleting Work associated with finalizer", "finalizer", dependenciesCleanupFinalizer)
		err := r.deleteWork(ctx, promise, logger)
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

	return fastRequeue, nil
}

func (r *PromiseReconciler) deleteDynamicControllerResources(ctx context.Context, promise *v1alpha1.Promise, logger logr.Logger) error {
	resourcesToDelete := map[schema.GroupVersion][]string{
		rbacv1.SchemeGroupVersion: {"ClusterRoleBinding", "ClusterRole", "RoleBinding", "Role"},
		v1.SchemeGroupVersion:     {"ServiceAccount", "ConfigMap"},
	}

	for gv, toDelete := range resourcesToDelete {
		for _, resource := range toDelete {
			gvk := schema.GroupVersionKind{
				Group:   gv.Group,
				Version: gv.Version,
				Kind:    resource,
			}
			resourcesRemaining, err := deleteAllResourcesWithKindMatchingLabel(ctx, r.Client, gvk, promise.GenerateSharedLabels(), logger)
			if err != nil {
				return err
			}

			if resourcesRemaining {
				return nil
			}
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
	resourcesRemaining, err := deleteAllResourcesWithKindMatchingLabel(ctx, r.Client, rrGVK, nil, logger)
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

func (r *PromiseReconciler) deleteCRDs(ctx context.Context, promise *v1alpha1.Promise, logger logr.Logger) error {
	crdGVK := schema.GroupVersionKind{
		Group:   apiextensionsv1.SchemeGroupVersion.Group,
		Version: apiextensionsv1.SchemeGroupVersion.Version,
		Kind:    "CustomResourceDefinition",
	}

	resourcesRemaining, err := deleteAllResourcesWithKindMatchingLabel(ctx, r.Client, crdGVK, promise.GenerateSharedLabels(), logger)
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

	resourcesRemaining, err := deleteAllResourcesWithKindMatchingLabel(ctx, r.Client, workGVK, promise.GenerateSharedLabels(), logger)
	if err != nil {
		return err
	}

	if !resourcesRemaining {
		controllerutil.RemoveFinalizer(promise, dependenciesCleanupFinalizer)
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

	err := json.Unmarshal(promise.Spec.API.Raw, rrCRD)
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

func (r *PromiseReconciler) createWorkResourceForDependencies(ctx context.Context, promise *v1alpha1.Promise, logger logr.Logger) error {
	workToCreate := &v1alpha1.Work{}
	workToCreate.Spec.Replicas = v1alpha1.DependencyReplicas
	workToCreate.Name = promise.GetIdentifier()
	workToCreate.Namespace = KratixSystemNamespace
	workToCreate.Labels = promise.GenerateSharedLabels()
	workToCreate.Spec.Scheduling.Promise = promise.Spec.Scheduling
	for _, u := range promise.Spec.Dependencies {
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

func (r *PromiseReconciler) generatePipelines(promise *v1alpha1.Promise, logger logr.Logger) ([]v1alpha1.Pipeline, []v1alpha1.Pipeline, error) {
	var configurePipelines []v1alpha1.Pipeline

	for _, pipeline := range promise.Spec.Workflows.Resource.Configure {
		p, err := generatePipeline(pipeline, logger)
		if err != nil {
			return nil, nil, err
		}
		configurePipelines = append(configurePipelines, p)
	}

	var deletePipelines []v1alpha1.Pipeline
	for _, pipeline := range promise.Spec.Workflows.Resource.Delete {
		p, err := generatePipeline(pipeline, logger)
		if err != nil {
			return nil, nil, err
		}
		deletePipelines = append(deletePipelines, p)
	}

	return configurePipelines, deletePipelines, nil
}

func generatePipeline(pipeline unstructured.Unstructured, logger logr.Logger) (v1alpha1.Pipeline, error) {
	pipelineLogger := logger.WithValues(
		"pipelineKind", pipeline.GetKind(),
		"pipelineVersion", pipeline.GetAPIVersion(),
		"pipelineName", pipeline.GetName())

	if pipeline.GetKind() == "Pipeline" && pipeline.GetAPIVersion() == "platform.kratix.io/v1alpha1" {
		jsonPipeline, err := pipeline.MarshalJSON()
		pipelineLogger.Info("json", "json", string(jsonPipeline))
		if err != nil {
			// TODO test
			pipelineLogger.Error(err, "Failed marshalling pipeline to json")
			return v1alpha1.Pipeline{}, err
		}

		p := v1alpha1.Pipeline{}
		err = json.Unmarshal(jsonPipeline, &p)
		if err != nil {
			// TODO test
			pipelineLogger.Error(err, "Failed unmarshalling pipeline")
			return v1alpha1.Pipeline{}, err
		}

		return p, nil
	}

	return v1alpha1.Pipeline{}, fmt.Errorf("unsupported pipeline %q (%s.%s)",
		pipeline.GetName(), pipeline.GetKind(), pipeline.GetAPIVersion())
}
