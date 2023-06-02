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

package v1alpha1

import (
	"github.com/syntasso/kratix/api/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/conversion"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// PromiseSpec defines the desired state of Promise
type PromiseSpec struct {

	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// TODO: apiextemnsion.CustomResourceDefinitionSpec struct(s) don't have the required jsontags and
	// cannot be used as a Type. See https://github.com/kubernetes-sigs/controller-tools/pull/528
	// && https://github.com/kubernetes-sigs/controller-tools/issues/291
	//
	// OPA Validation pattern:
	// https://github.com/open-policy-agent/frameworks/blob/1307ba72bce38ee3cf44f94def1bbc41eb4ffa90/constraint/pkg/apis/templates/v1beta1/constrainttemplate_types.go#L46
	// XaasCrd runtime.RawExtension      `json:"xaasCrd,omitempty"`

	// X's CustomResourceDefinition to create the X-aaS offering
	//
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:EmbeddedResource
	XaasCrd runtime.RawExtension `json:"xaasCrd,omitempty"`

	// Array of Image tags to transform from input request custom resource to output resource(s)
	XaasRequestPipeline []string `json:"xaasRequestPipeline,omitempty"`

	WorkerClusterResources []WorkerClusterResource `json:"workerClusterResources,omitempty"`

	ClusterSelector map[string]string `json:"clusterSelector,omitempty"`
}

// Resources represents the manifest workload to be deployed on worker cluster
type WorkerClusterResource struct {
	// Manifests represents a list of kubernetes resources to be deployed on the worker cluster.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	unstructured.Unstructured `json:",inline"`
}

// PromiseStatus defines the observed state of Promise
type PromiseStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// Promise is the Schema for the promises API
type Promise struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PromiseSpec   `json:"spec,omitempty"`
	Status PromiseStatus `json:"status,omitempty"`
}

func (p *Promise) DoesNotContainXAASCrd() bool {
	// if a request pipeline is set but there is not a CRD the pipeline is ignored
	// TODO how can we prevent this scenario from happening
	return p.Spec.XaasCrd.Raw == nil
}

func (p *Promise) GenerateSharedLabels() map[string]string {
	return map[string]string{
		"kratix-promise-id": p.GetIdentifier(),
	}
}
func (p *Promise) GetIdentifier() string {
	return p.GetName() + "-" + p.GetNamespace()
}

func (p *Promise) GetControllerResourceName() string {
	return p.GetIdentifier() + "-promise-controller"
}

func (p *Promise) GetPipelineResourceName() string {
	return p.GetIdentifier() + "-promise-pipeline"
}

func (p *Promise) GetConfigMapName() string {
	return "cluster-selectors-" + p.GetIdentifier()
}

func (p *Promise) GetPipelineResourceNamespace() string {
	return "default"
}

//+kubebuilder:object:root=true

// PromiseList contains a list of Promise
type PromiseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Promise `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Promise{}, &PromiseList{})
}

var logger = ctrl.Log.WithName("webhook")

// v1alpha1                      v1beta1
func (src *Promise) ConvertTo(dstRaw conversion.Hub) error {
	//update dstRaw to have the fields

	dst := dstRaw.(*v1beta1.Promise)
	logger.Info("converting object from v1alpha1 to v1beta1", "oldObj", src)

	dst.Spec.Pipeline = src.Spec.XaasRequestPipeline
	dst.Spec.XaasCrd = src.Spec.XaasCrd
	//fix this and keep converting the resource
	// - testing gitops drift detection with converison webhooks
	for _, wcr := range src.Spec.WorkerClusterResources {
		dst.Spec.WorkerClusterResources = append(dst.Spec.WorkerClusterResources, v1beta1.WorkerClusterResource{Unstructured: wcr.Unstructured})
	}

	dst.Spec.ClusterSelector = src.Spec.ClusterSelector

	dst.ObjectMeta = src.ObjectMeta

	logger.Info("conversion from v1alpha1 to v1beta1 complete", "newObj", src)
	logger.Info("\n")

	return nil
}

// v1beta1                        v1alpha1
func (dst *Promise) ConvertFrom(srcRaw conversion.Hub) error {
	src := srcRaw.(*v1beta1.Promise)
	logger.Info("converting object from v1beta1 to v1alpha1", "oldObj", src)

	dst.ObjectMeta = src.ObjectMeta

	dst.Spec.XaasRequestPipeline = src.Spec.Pipeline
	dst.Spec.XaasCrd = src.Spec.XaasCrd
	//fix this and keep converting the resource
	// - testing gitops drift detection with converison webhooks
	for _, wcr := range src.Spec.WorkerClusterResources {
		dst.Spec.WorkerClusterResources = append(dst.Spec.WorkerClusterResources, WorkerClusterResource{Unstructured: wcr.Unstructured})
	}

	dst.Spec.ClusterSelector = src.Spec.ClusterSelector

	dst.ObjectMeta = src.ObjectMeta

	logger.Info("conversion from v1beta1 to v1alpha1 complete", "newObj", src)
	logger.Info("\n")
	return nil
}
