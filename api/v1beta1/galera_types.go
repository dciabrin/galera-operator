/*
Copyright 2022.

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

package v1beta1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GaleraSpec defines the desired state of Galera
type GaleraSpec struct {
	// Name of the secret to look for password keys
	Secret string `json:"secret,omitempty"`
	// Storage class to host the mariadb databases
	StorageClass string `json:"storageClass,omitempty"`
	// Storage size allocated for the mariadb databases
	StorageRequest string `json:"storageRequest,omitempty"`
	// Name of the galera container image to run
	ContainerImage string `json:"containerImage"`
	// +kubebuilder:validation:Minimum=1
	// Size of the galera cluster deployment
	Size int32 `json:"size"`
}

type GaleraAttributes struct {
	// Last recorded replication sequence number in the DB
	Seqno string `json:"seqno"`
	// URI used to connect to the galera cluster
	Gcomm string `json:"gcomm,omitempty"`
}

// GaleraStatus defines the observed state of Galera
type GaleraStatus struct {
	// A map of database node attributes for each pod
	Attributes map[string]GaleraAttributes `json:"attributes,omitempty"`
	// Name of the node that can safely bootstrap a cluster
	SafeToBootstrap string `json:"safeToBootstrap,omitempty"`
	// Is the galera cluster currently running
	Bootstrapped bool `json:"bootstrapped"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// Galera is the Schema for the galeras API
type Galera struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GaleraSpec   `json:"spec,omitempty"`
	Status GaleraStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// GaleraList contains a list of Galera
type GaleraList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Galera `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Galera{}, &GaleraList{})
}
