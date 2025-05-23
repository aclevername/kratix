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

// PipelineTriggerSpec defines the desired state of PipelineTrigger.
type PipelineTriggerSpec struct {
	ResourceName      string `json:"resourceName"`
	ResourceNamespace string `json:"resourceNamespace"`
	APIVersion        string `json:"apiVersion"`
	Kind              string `json:"kind"`
	NextPipeline      string `json:"nextPipeline"`
	UID               string `json:"uid"`
	Triggered         bool   `json:"triggered"`
}

// PipelineTriggerStatus defines the observed state of PipelineTrigger.
type PipelineTriggerStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// PipelineTrigger is the Schema for the pipelinetriggers API.
type PipelineTrigger struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PipelineTriggerSpec   `json:"spec,omitempty"`
	Status PipelineTriggerStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PipelineTriggerList contains a list of PipelineTrigger.
type PipelineTriggerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PipelineTrigger `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PipelineTrigger{}, &PipelineTriggerList{})
}
