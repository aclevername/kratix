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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// CompoundRequestMetadataSpec defines the desired state of CompoundRequestMetadata
type CompoundRequestMetadataSpec struct {
	Resources []ResReqRef `json:"resources"`
}

type ResReqRef struct {
	// Name of the resource
	// +required
	Name string `json:"name"`
	// Namespace of the resource
	// +required
	Namespace string `json:"namespace,omitempty"`
	// APIVersion of the resource
	// +required
	APIVersion string `json:"group"`
	// Kind of the resource
	// +required
	Kind string `json:"kind"`
}

// CompoundRequestMetadataStatus defines the observed state of CompoundRequestMetadata.
type CompoundRequestMetadataStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// For Kubernetes API conventions, see:
	// https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties

	// conditions represent the current state of the CompoundRequestMetadata resource.
	// Each condition has a unique type and reflects the status of a specific aspect of the resource.
	//
	// Standard condition types include:
	// - "Available": the resource is fully functional
	// - "Progressing": the resource is being created or updated
	// - "Degraded": the resource failed to reach or maintain its desired state
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// CompoundRequestMetadata is the Schema for the compoundrequestmetadata API
type CompoundRequestMetadata struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of CompoundRequestMetadata
	// +required
	Spec CompoundRequestMetadataSpec `json:"spec"`

	// status defines the observed state of CompoundRequestMetadata
	// +optional
	Status CompoundRequestMetadataStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// CompoundRequestMetadataList contains a list of CompoundRequestMetadata
type CompoundRequestMetadataList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CompoundRequestMetadata `json:"items"`
}

func init() {
	SchemeBuilder.Register(&CompoundRequestMetadata{}, &CompoundRequestMetadataList{})
}
