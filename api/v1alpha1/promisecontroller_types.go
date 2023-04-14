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

// PromiseControllerSpec defines the desired state of PromiseController
type PromiseControllerSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// Foo is an example field of PromiseController. Edit promisecontroller_types.go to remove/update
	ClusterSelector map[string]string `json:"clusterSelector,omitempty"`
	Pipelines       []string          `json:"pipelines,omitempty"`
	UID             string            `json:"uid,omitempty"`
	Group           string            `json:"group,omitempty"`
	Version         string            `json:"version,omitempty"`
	Kind            string            `json:"kind,omitempty"`
	Plural          string            `json:"plural,omitempty"`
}

// PromiseControllerStatus defines the observed state of PromiseController
type PromiseControllerStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// PromiseController is the Schema for the promisecontrollers API
type PromiseController struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PromiseControllerSpec   `json:"spec,omitempty"`
	Status PromiseControllerStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// PromiseControllerList contains a list of PromiseController
type PromiseControllerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PromiseController `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PromiseController{}, &PromiseControllerList{})
}
