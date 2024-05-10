package controllers

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/bitly/go-simplejson"
	"github.com/go-logr/logr"
	genapi "github.com/grafana/grafana-openapi-client-go/client"
	"github.com/grafana/grafana-openapi-client-go/client/orgs"
	"github.com/grafana/grafana-openapi-client-go/models"
	"github.com/grafana/grafana-operator/v5/api/v1beta1"
	client2 "github.com/grafana/grafana-operator/v5/controllers/client"
	"github.com/grafana/grafana-operator/v5/controllers/metrics"
	kuberr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"strings"
	"time"
)

// GrafanaOrganizationReconciler reconciles a GrafanaOrganization object
type GrafanaOrganizationReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Log    logr.Logger
}

//+kubebuilder:rbac:groups=grafana.integreatly.org,resources=grafanaorganizations,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=grafana.integreatly.org,resources=grafanaorganizations/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=grafana.integreatly.org,resources=grafanaorganizations/finalizers,verbs=update

func (r *GrafanaOrganizationReconciler) syncOrganizations(ctx context.Context) (ctrl.Result, error) {
	syncLog := log.FromContext(ctx).WithName("GrafanaOrganizationReconciler")
	syncLog.Info("syncOrganizations")
	organizationsSynced := 0

	// get all grafana instances
	grafanas := &v1beta1.GrafanaList{}
	var opts []client.ListOption
	err := r.Client.List(ctx, grafanas, opts...)
	if err != nil {
		return ctrl.Result{
			Requeue: true,
		}, err
	}

	// no instances, no need to sync
	if len(grafanas.Items) == 0 {
		return ctrl.Result{Requeue: false}, nil
	}

	// get all organizations
	allOrganizations := &v1beta1.GrafanaOrganizationList{}
	err = r.Client.List(ctx, allOrganizations, opts...)
	if err != nil {
		return ctrl.Result{
			Requeue: true,
		}, err
	}

	// sync organizations, delete organizations from grafana that do no longer have a cr
	organizationsToDelete := map[*v1beta1.Grafana][]v1beta1.NamespacedResource{}
	for _, grafana := range grafanas.Items {
		grafana := grafana

		for _, organization := range grafana.Status.Organizations {
			if allOrganizations.Find(organization.Namespace(), organization.Name()) == nil {
				organizationsToDelete[&grafana] = append(organizationsToDelete[&grafana], organization)
			}
		}
	}

	// delete all organizations that no longer have a cr
	for grafana, existingOrganizations := range organizationsToDelete {
		grafana := grafana
		grafanaClient, err := client2.NewGeneratedGrafanaClient(ctx, r.Client, grafana)
		if err != nil {
			return ctrl.Result{Requeue: true}, err
		}

		for _, organization := range existingOrganizations {
			// avoid bombarding the grafana instance with a large number of requests at once, limit
			// the sync to ten organizations per cycle. This means that it will take longer to sync
			// a large number of deleted organization crs, but that should be an edge case.
			if organizationsSynced >= syncBatchSize {
				return ctrl.Result{Requeue: true}, nil
			}

			namespace, name, orgName := organization.Split()
			instanceOrganization, err := grafanaClient.Orgs.GetOrgByName(orgName)
			if err != nil {
				if !strings.Contains(err.Error(), "Organization not found") {
					return ctrl.Result{Requeue: false}, err
				}
				syncLog.Info("organization no longer exists", "namespace", namespace, "name", name)
			} else {
				_, err = grafanaClient.Orgs.DeleteOrgByID(instanceOrganization.Payload.ID) //nolint
				if err != nil {
					var notFound *orgs.DeleteOrgByIDNotFound
					if !errors.As(err, &notFound) {
						return ctrl.Result{Requeue: false}, err
					}
				}
			}

			grafana.Status.Organizations = grafana.Status.Organizations.Remove(namespace, name)
			organizationsSynced += 1

		}

		// one update per grafana - this will trigger a reconcile of the grafana controller
		// so we should minimize those updates
		err = r.Client.Status().Update(ctx, grafana)
		if err != nil {
			return ctrl.Result{Requeue: false}, err
		}

	}

	if organizationsSynced > 0 {
		syncLog.Info("successfully synced organizations", "organizations", organizationsSynced)
	}

	return ctrl.Result{}, nil

}

func (r *GrafanaOrganizationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	controllerLog := log.FromContext(ctx).WithName("GrafanaOrganizationReconciler")
	r.Log = controllerLog

	// periodic sync reconcile
	if req.Namespace == "" && req.Name == "" {
		start := time.Now()
		syncResult, err := r.syncOrganizations(ctx)
		elapsed := time.Since(start).Milliseconds()
		metrics.InitialOrganisationsSyncDuration.Set(float64(elapsed))
		return syncResult, err
	}

	cr := &v1beta1.GrafanaOrganization{}
	err := r.Client.Get(ctx, client.ObjectKey{
		Namespace: req.Namespace,
		Name:      req.Name,
	}, cr)
	if err != nil {
		if kuberr.IsNotFound(err) {
			err = r.onOrganizationDeleted(ctx, req.Namespace, req.Name)
			if err != nil {
				return ctrl.Result{RequeueAfter: RequeueDelay}, err
			}
			return ctrl.Result{}, nil
		}
		controllerLog.Error(err, "error getting grafana organization cr")
		return ctrl.Result{RequeueAfter: RequeueDelay}, err
	}

	if cr.Spec.Organization == nil {
		controllerLog.Info("skipped organization with empty spec", cr.Name, cr.Namespace)
		// TODO: add a custom status around that?
		return ctrl.Result{}, nil
	}

	instances, err := r.GetMatchingOrganizationInstances(ctx, cr, r.Client)
	if err != nil {
		controllerLog.Error(err, "could not find matching instances", "name", cr.Name, "namespace", cr.Namespace)
		return ctrl.Result{RequeueAfter: RequeueDelay}, err
	}

	controllerLog.Info("found matching Grafana instances for organization", "count", len(instances.Items))

	organization, hash, err := r.getOrganizationContent(ctx, cr)
	if err != nil {
		controllerLog.Error(err, "could not retrieve organization contents", "name", cr.Name, "namespace", cr.Namespace)
		return ctrl.Result{RequeueAfter: RequeueDelay}, err
	}

	success := true
	for _, grafana := range instances.Items {
		// check if this is a cross namespace import
		if grafana.Namespace != cr.Namespace && !cr.IsAllowCrossNamespaceImport() {
			continue
		}

		grafana := grafana
		// an admin url is required to interact with grafana
		// the instance or route might not yet be ready
		if grafana.Status.Stage != v1beta1.OperatorStageComplete || grafana.Status.StageStatus != v1beta1.OperatorStageResultSuccess {
			controllerLog.Info("grafana instance not ready", "grafana", grafana.Name)
			success = false
			continue
		}

		// then import the organization into the matching grafana instances
		err = r.onOrganizationCreated(ctx, &grafana, cr, organization, hash)
		if err != nil {
			success = false
			cr.Status.LastMessage = err.Error()
			controllerLog.Error(err, "error reconciling organization", "organization", cr.Name, "grafana", grafana.Name)
		}
	}

	// if the organization was successfully synced in all instances, wait for its re-sync period
	if success {
		cr.Status.LastMessage = ""
		cr.Status.Hash = hash
		if cr.ResyncPeriodHasElapsed() {
			cr.Status.LastResync = metav1.Time{Time: time.Now()}
		}
		cr.Status.UID = organization.Name
		return ctrl.Result{RequeueAfter: cr.GetResyncPeriod()}, r.Client.Status().Update(ctx, cr)
	} else {
		// if there was an issue with the organization, update the status
		return ctrl.Result{RequeueAfter: RequeueDelay}, r.Client.Status().Update(ctx, cr)
	}

}

func (r *GrafanaOrganizationReconciler) onOrganizationDeleted(ctx context.Context, namespace string, name string) error {
	log := log.FromContext(ctx).WithName("GrafanaOrganizationReconciler")
	log.Info("onOrganizationDeleted")
	list := v1beta1.GrafanaList{}
	opts := []client.ListOption{}
	err := r.Client.List(ctx, &list, opts...)
	if err != nil {
		return err
	}

	for _, grafana := range list.Items {
		grafana := grafana
		if found, orgName := grafana.Status.Organizations.Find(namespace, name); found {
			grafanaClient, err := client2.NewGeneratedGrafanaClient(ctx, r.Client, &grafana)
			if err != nil {
				return err
			}

			orgInstance, err := grafanaClient.Orgs.GetOrgByName(*orgName)
			if err != nil {
				if !strings.Contains(err.Error(), "Organization not found") {
					return err
				}
			} else if orgInstance != nil {
				_, err = grafanaClient.Orgs.DeleteOrgByID(orgInstance.Payload.ID) //nolint
				if err != nil {
					if !strings.Contains(err.Error(), "ID not found") {
						return err
					}
				}
			}

			grafana.Status.Organizations = grafana.Status.Organizations.Remove(namespace, name)
			return r.Client.Status().Update(ctx, &grafana)
		}
	}

	return nil

}

func (r *GrafanaOrganizationReconciler) onOrganizationCreated(ctx context.Context, grafana *v1beta1.Grafana, cr *v1beta1.GrafanaOrganization, organization *models.UpdateOrgForm, hash string) error {

	if cr.Spec.Organization == nil {
		return nil
	}

	grafanaClient, err := client2.NewGeneratedGrafanaClient(ctx, r.Client, grafana)
	if err != nil {
		return err
	}

	exists, id, err := r.Exists(grafanaClient, organization.Name)
	if err != nil {
		return err
	}

	if exists && cr.Unchanged(hash) && !cr.ResyncPeriodHasElapsed() {
		return nil
	}

	encoded, err := json.Marshal(organization)
	if err != nil {
		return fmt.Errorf("representing organization as JSON: %w", err)
	}
	if exists {
		var body models.UpdateOrgForm
		if err := json.Unmarshal(encoded, &body); err != nil {
			return fmt.Errorf("representing organization as update command: %w", err)
		}
		//organization.UID = uid
		_, err := grafanaClient.Orgs.UpdateOrg(id, &body)
		//_, err := grafanaClient.Datasources.UpdateDataSourceByUID(organization.UID, &body) //nolint
		if err != nil {
			return err
		}
	} else {
		var body models.CreateOrgCommand

		if err := json.Unmarshal(encoded, &body); err != nil {
			return fmt.Errorf("representing organization as create command: %w", err)
		}
		orgCreatedResponse, err := grafanaClient.Orgs.CreateOrg(&body) //nolint
		if err != nil {
			return err
		}
		id = *orgCreatedResponse.Payload.OrgID
	}

	grafana.Status.Organizations = grafana.Status.Organizations.Add(cr.Namespace, cr.Name, organization.Name)
	return r.Client.Status().Update(ctx, grafana)

}

func (r *GrafanaOrganizationReconciler) Exists(client *genapi.GrafanaHTTPAPI, name string) (bool, int64, error) {
	organization, err := client.Orgs.GetOrgByName(name)
	if err != nil && !strings.Contains(err.Error(), "(status 404)") {
		return false, 0, fmt.Errorf("fetching organization: %w", err)
	}

	if organization != nil {
		return true, organization.Payload.ID, nil
	}

	return false, 0, nil

}

func (r *GrafanaOrganizationReconciler) SetupWithManager(mgr ctrl.Manager, ctx context.Context) error {
	err := ctrl.NewControllerManagedBy(mgr).
		For(&v1beta1.GrafanaOrganization{}).
		Complete(r)

	if err == nil {
		d, err := time.ParseDuration(initialSyncDelay)
		if err != nil {
			return err
		}

		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-time.After(d):
					result, err := r.Reconcile(ctx, ctrl.Request{})
					if err != nil {
						r.Log.Error(err, "error synchronizing organizations")
						continue
					}
					if result.Requeue {
						r.Log.Info("more organizations left to synchronize")
						continue
					}
					r.Log.Info("organizations sync complete")
					return
				}
			}
		}()
	}
	return err
}

func (r *GrafanaOrganizationReconciler) GetMatchingOrganizationInstances(ctx context.Context, organization *v1beta1.GrafanaOrganization, k8sClient client.Client) (v1beta1.GrafanaList, error) {
	instances, err := GetMatchingInstances(ctx, k8sClient, organization.Spec.InstanceSelector)
	if err != nil || len(instances.Items) == 0 {
		organization.Status.NoMatchingInstances = true
		if err := r.Client.Status().Update(ctx, organization); err != nil {
			r.Log.Info("unable to update the status of %v, in %v", organization.Name, organization.Namespace)
		}
		return v1beta1.GrafanaList{}, err
	}
	organization.Status.NoMatchingInstances = false
	if err := r.Client.Status().Update(ctx, organization); err != nil {
		r.Log.Info("unable to update the status of %v, in %v", organization.Name, organization.Namespace)
	}

	return instances, err
}

func (r *GrafanaOrganizationReconciler) getOrganizationContent(ctx context.Context, cr *v1beta1.GrafanaOrganization) (*models.UpdateOrgForm, string, error) {
	initialBytes, err := json.Marshal(cr.Spec.Organization)
	if err != nil {
		return nil, "", err
	}

	simpleContent, err := simplejson.NewJson(initialBytes)
	if err != nil {
		return nil, "", err
	}

	/*if cr.Spec.Organization.UID == "" {
		simpleContent.Set("uid", string(cr.UID))
	}*/

	newBytes, err := simpleContent.MarshalJSON()
	if err != nil {
		return nil, "", err
	}

	// We use UpdateOrgForm here because models.DataSource lacks the SecureJsonData field
	var res models.UpdateOrgForm
	if err = json.Unmarshal(newBytes, &res); err != nil {
		return nil, "", err
	}

	hash := sha256.New()
	hash.Write(newBytes)

	return &res, fmt.Sprintf("%x", hash.Sum(nil)), nil
}
