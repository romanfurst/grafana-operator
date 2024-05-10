package v1beta1

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"time"
)

type GrafanaOrganizationInternal struct {
	Name string `json:"name,omitempty"`

	// +kubebuilder:validation:Schemaless
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Type=object
	// +optional
	JSONData json.RawMessage `json:"jsonData,omitempty"`

	// +kubebuilder:validation:Schemaless
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Type=object
	// +optional
	SecureJSONData json.RawMessage `json:"secureJsonData,omitempty"`
}

// GrafanaOrganizationSpec defines the desired state of GrafanaOrganization
type GrafanaOrganizationSpec struct {
	Organization *GrafanaOrganizationInternal `json:"organization"`

	// selects Grafana instances for import
	InstanceSelector *metav1.LabelSelector `json:"instanceSelector"`

	// how often the organization is refreshed, defaults to 5m if not set
	// +optional
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Format=duration
	// +kubebuilder:validation:Pattern="^([0-9]+(\\.[0-9]+)?(ns|us|µs|ms|s|m|h))+$"
	// +kubebuilder:default="5m"
	ResyncPeriod string `json:"resyncPeriod,omitempty"`

	// allow to import this resources from an operator in a different namespace
	// +optional
	AllowCrossNamespaceImport *bool `json:"allowCrossNamespaceImport,omitempty"`
}

// GrafanaOrganizationStatus defines the observed state of GrafanaOrganization
type GrafanaOrganizationStatus struct {
	Hash        string `json:"hash,omitempty"`
	LastMessage string `json:"lastMessage,omitempty"`
	// The organization instanceSelector can't find matching grafana instances
	NoMatchingInstances bool `json:"NoMatchingInstances,omitempty"`
	// Last time the organization was resynced
	LastResync metav1.Time `json:"lastResync,omitempty"`
	UID        string      `json:"uid,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// GrafanaOrganization is the Schema for the grafanaorganizations API
// +kubebuilder:printcolumn:name="No matching instances",type="boolean",JSONPath=".status.NoMatchingInstances",description=""
// +kubebuilder:printcolumn:name="Last resync",type="date",format="date-time",JSONPath=".status.lastResync",description=""
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description=""
type GrafanaOrganization struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GrafanaOrganizationSpec   `json:"spec,omitempty"`
	Status GrafanaOrganizationStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// GrafanaOrganizationList contains a list of GrafanaOrganization
type GrafanaOrganizationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GrafanaOrganization `json:"items"`
}

func (in *GrafanaOrganization) GetResyncPeriod() time.Duration {
	if in.Spec.ResyncPeriod == "" {
		in.Spec.ResyncPeriod = DefaultResyncPeriod
		return in.GetResyncPeriod()
	}

	duration, err := time.ParseDuration(in.Spec.ResyncPeriod)
	if err != nil {
		in.Spec.ResyncPeriod = DefaultResyncPeriod
		return in.GetResyncPeriod()
	}

	return duration
}

func (in *GrafanaOrganization) ResyncPeriodHasElapsed() bool {
	deadline := in.Status.LastResync.Add(in.GetResyncPeriod())
	return time.Now().After(deadline)
}

func (in *GrafanaOrganization) Unchanged(hash string) bool {
	return in.Status.Hash == hash
}

func (in *GrafanaOrganization) ExpandVariables(variables map[string][]byte) ([]byte, error) {
	if in.Spec.Organization == nil {
		return nil, errors.New("data source is empty, can't expand variables")
	}

	raw, err := json.Marshal(in.Spec.Organization)
	if err != nil {
		return nil, err
	}

	for key, value := range variables {
		patterns := []string{fmt.Sprintf("$%v", key), fmt.Sprintf("${%v}", key)}
		for _, pattern := range patterns {
			raw = bytes.ReplaceAll(raw, []byte(pattern), value)
		}
	}

	return raw, nil
}

func (in *GrafanaOrganization) IsAllowCrossNamespaceImport() bool {
	if in.Spec.AllowCrossNamespaceImport != nil {
		return *in.Spec.AllowCrossNamespaceImport
	}
	return false
}

func (in *GrafanaOrganizationList) Find(namespace string, name string) *GrafanaOrganization {
	for _, organization := range in.Items {
		if organization.Namespace == namespace && organization.Name == name {
			return &organization
		}
	}
	return nil
}

func init() {
	SchemeBuilder.Register(&GrafanaOrganization{}, &GrafanaOrganizationList{})
}
