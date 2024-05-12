package v1beta1

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"time"
)

type GrafanaUserInternal struct {
	Name     string `json:"name,omitempty"`
	Email    string `json:"email,omitempty"`
	Login    string `json:"login,omitempty"`
	Password string `json:"password,omitempty"`

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

// GrafanaUserSpec defines the desired state of GrafanaUser
type GrafanaUserSpec struct {
	User *GrafanaUserInternal `json:"user"`

	// selects Grafana instances for import
	InstanceSelector *metav1.LabelSelector `json:"instanceSelector"`

	// how often the user is refreshed, defaults to 5m if not set
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

// GrafanaUserStatus defines the observed state of GrafanaUser
type GrafanaUSerStatus struct {
	Hash        string `json:"hash,omitempty"`
	LastMessage string `json:"lastMessage,omitempty"`
	// The user instanceSelector can't find matching grafana instances
	NoMatchingInstances bool `json:"NoMatchingInstances,omitempty"`
	// Last time the user was resynced
	LastResync metav1.Time `json:"lastResync,omitempty"`
	UID        string      `json:"uid,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// GrafanaUser is the Schema for the grafanausers API
// +kubebuilder:printcolumn:name="No matching instances",type="boolean",JSONPath=".status.NoMatchingInstances",description=""
// +kubebuilder:printcolumn:name="Last resync",type="date",format="date-time",JSONPath=".status.lastResync",description=""
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description=""
type GrafanaUser struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GrafanaUserSpec   `json:"spec,omitempty"`
	Status GrafanaUSerStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// GrafanaUserList contains a list of GrafanaUsere
type GrafanaUserList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GrafanaUser `json:"items"`
}

func (in *GrafanaUser) GetResyncPeriod() time.Duration {
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

func (in *GrafanaUser) ResyncPeriodHasElapsed() bool {
	deadline := in.Status.LastResync.Add(in.GetResyncPeriod())
	return time.Now().After(deadline)
}

func (in *GrafanaUser) Unchanged(hash string) bool {
	return in.Status.Hash == hash
}

func (in *GrafanaUser) IsUpdatedUID(uid string) bool {
	// User has just been created, status is not yet updated
	if in.Status.UID == "" {
		return false
	}

	if uid == "" {
		uid = string(in.UID)
	}

	return in.Status.UID != uid
}

func (in *GrafanaUser) ExpandVariables(variables map[string][]byte) ([]byte, error) {
	if in.Spec.User == nil {
		return nil, errors.New("user is empty, can't expand variables")
	}

	raw, err := json.Marshal(in.Spec.User)
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

func (in *GrafanaUser) IsAllowCrossNamespaceImport() bool {
	if in.Spec.AllowCrossNamespaceImport != nil {
		return *in.Spec.AllowCrossNamespaceImport
	}
	return false
}

func (in *GrafanaUserList) Find(namespace string, name string) *GrafanaUser {
	for _, user := range in.Items {
		if user.Namespace == namespace && user.Name == name {
			return &user
		}
	}
	return nil
}

func init() {
	SchemeBuilder.Register(&GrafanaUser{}, &GrafanaUserList{})
}
