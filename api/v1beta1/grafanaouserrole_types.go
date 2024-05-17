package v1beta1

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"time"
)

type GrafanaUserRoleInternal struct {
	LoginOrEmail string `json:"loginOrEmail,omitempty"`
	OrgName      string `json:"orgName,omitempty"`
	Role         string `json:"role,omitempty"`

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

// GrafanaUserRoleSpec defines the desired state of GrafanaUserRole
type GrafanaUserRoleSpec struct {
	UserRole *GrafanaUserRoleInternal `json:"userrole"`

	// selects Grafana instances for import
	InstanceSelector *metav1.LabelSelector `json:"instanceSelector"`

	// how often the userrole is refreshed, defaults to 5m if not set
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

// GrafanaUserRoleStatus defines the observed state of GrafanaUserRole
type GrafanaUserRoleStatus struct {
	Hash        string `json:"hash,omitempty"`
	LastMessage string `json:"lastMessage,omitempty"`
	// The userrole instanceSelector can't find matching grafana instances
	NoMatchingInstances bool `json:"NoMatchingInstances,omitempty"`
	// Last time the userrole was resynced
	LastResync metav1.Time `json:"lastResync,omitempty"`
	UID        string      `json:"uid,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// GrafanaUserRole is the Schema for the grafanauserroles API
// +kubebuilder:printcolumn:name="No matching instances",type="boolean",JSONPath=".status.NoMatchingInstances",description=""
// +kubebuilder:printcolumn:name="Last resync",type="date",format="date-time",JSONPath=".status.lastResync",description=""
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description=""
type GrafanaUserRole struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GrafanaUserRoleSpec   `json:"spec,omitempty"`
	Status GrafanaUserRoleStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// GrafanaUserRoleList contains a list of GrafanaUserRole
type GrafanaUserRoleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GrafanaUserRole `json:"items"`
}

func (in *GrafanaUserRole) GetResyncPeriod() time.Duration {
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

func (in *GrafanaUserRole) ResyncPeriodHasElapsed() bool {
	deadline := in.Status.LastResync.Add(in.GetResyncPeriod())
	return time.Now().After(deadline)
}

func (in *GrafanaUserRole) Unchanged(hash string) bool {
	return in.Status.Hash == hash
}

func (in *GrafanaUserRole) IsUpdatedUID(uid string) bool {
	// UserRole has just been created, status is not yet updated
	if in.Status.UID == "" {
		return false
	}

	if uid == "" {
		uid = string(in.UID)
	}

	return in.Status.UID != uid
}

func (in *GrafanaUserRole) ExpandVariables(variables map[string][]byte) ([]byte, error) {
	if in.Spec.UserRole == nil {
		return nil, errors.New("data source is empty, can't expand variables")
	}

	raw, err := json.Marshal(in.Spec.UserRole)
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

func (in *GrafanaUserRole) IsAllowCrossNamespaceImport() bool {
	if in.Spec.AllowCrossNamespaceImport != nil {
		return *in.Spec.AllowCrossNamespaceImport
	}
	return false
}

func (in *GrafanaUserRoleList) Find(namespace string, name string) *GrafanaUserRole {
	for _, userrole := range in.Items {
		if userrole.Namespace == namespace && userrole.Name == name {
			return &userrole
		}
	}
	return nil
}

func init() {
	SchemeBuilder.Register(&GrafanaUserRole{}, &GrafanaUserRoleList{})
}
