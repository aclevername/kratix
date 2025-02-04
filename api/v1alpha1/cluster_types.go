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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type StateStoreCoreFields struct {
	// Path within the StateStore to write documents. This path should be allocated
	// to Kratix as it will create, update, and delete files within this path.
	// Path structure begins with provided path and ends with namespaced cluster name:
	//   <StateStore.Spec.Path>/<Cluster.Spec.Path>/<Cluster.Metadata.Namespace>/<Cluster.Metadata.Name>/
	//+kubebuilder:validation:Optional
	Path string `json:"path,omitempty"`
	// SecretRef specifies the Secret containing authentication credentials
	SecretRef *corev1.LocalObjectReference `json:"secretRef,omitempty"`
}

// ClusterSpec defines the desired state of Cluster
type ClusterSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// Path within StateStore to write documents, this will be appended to any
	// specficed Spec.Path provided in the referenced StateStore.
	// Kratix will then namespace any resources within the provided path.
	// Path structure will be:
	//   <StateStore.Spec.Path>/<Cluster.Spec.Path>/<Cluster.Metadata.Namespace>/<Cluster.Metadata.Name>/
	//+kubebuilder:validation:Optional
	StateStoreCoreFields `json:",inline"`
	StateStoreRef        *StateStoreReference `json:"stateStoreRef,omitempty"`
}

// ClusterStatus defines the observed state of Cluster
type ClusterStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// Cluster is the Schema for the clusters API
type Cluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ClusterSpec   `json:"spec,omitempty"`
	Status ClusterStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// ClusterList contains a list of Cluster
type ClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Cluster `json:"items"`
}

// StateStoreReference is a reference to a StateStore
type StateStoreReference struct {
	// +kubebuilder:validation:Enum=BucketStateStore;GitStateStore
	Kind string `json:"kind"`
	Name string `json:"name"`
}

func init() {
	SchemeBuilder.Register(&Cluster{}, &ClusterList{})
}
