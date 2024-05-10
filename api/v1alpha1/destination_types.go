/*
Copyright 2021 Syntasso.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

distributed under the License is distributed on an "AS IS" BASIS,
Unless required by applicable law or agreed to in writing, software
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
	// Path structure begins with provided path and ends with namespaced destination name:
	//   <StateStore.Spec.Path>/<Destination.Spec.Path>/<Destination.Metadata.Namespace>/<Destination.Metadata.Name>/
	//+kubebuilder:validation:Optional
	Path string `json:"path,omitempty"`
	// SecretRef specifies the Secret containing authentication credentials
	SecretRef *corev1.SecretReference `json:"secretRef,omitempty"`
}

// TODO: revisit if we want all destination secrets on a single known namespaces
// (i.e. kratix-platform-system) or if we want to allow users to specify a
// namespace for each destination secret.

// DestinationSpec defines the desired state of Destination
type DestinationSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of destination
	// Important: Run "make" to regenerate code after modifying this file

	// Path within StateStore to write documents, this will be appended to any
	// specficed Spec.Path provided in the referenced StateStore.
	// Kratix will then namespace any resources within the provided path.
	// Path structure will be:
	//   <StateStore.Spec.Path>/<Destination.Spec.Path>/<Destination.Metadata.Namespace>/<Destination.Metadata.Name>/
	//+kubebuilder:validation:Optional
	StateStoreCoreFields `json:",inline"`
	StateStoreRef        *StateStoreReference `json:"stateStoreRef,omitempty"`

	// By default, Kratix will schedule works without labels to all destinations
	// (for promise dependencies) or to a random destination (for resource
	// requests). If StrictMatchLabels is true, Kratix will only schedule works
	// to this destination if it can be selected by the Promise's
	// destinationSelectors. An empty label set on the work won't be scheduled
	// to this destination, unless the destination label set is also empty
	// +kubebuilder:validation:Optional
	StrictMatchLabels bool              `json:"strictMatchLabels,omitempty"`
	Variables         map[string]string `json:"variables,omitempty"`
}

// DestinationStatus defines the observed state of Destination
type DestinationStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of destination
	// Important: Run "make" to regenerate code after modifying this file
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:scope=Cluster,path=destinations,categories=kratix

// Destination is the Schema for the Destinations API
type Destination struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DestinationSpec   `json:"spec,omitempty"`
	Status DestinationStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// DestinationList contains a list of Destination
type DestinationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Destination `json:"items"`
}

// StateStoreReference is a reference to a StateStore
type StateStoreReference struct {
	// +kubebuilder:validation:Enum=BucketStateStore;GitStateStore
	Kind string `json:"kind"`
	Name string `json:"name"`
}

func init() {
	SchemeBuilder.Register(&Destination{}, &DestinationList{})
}
