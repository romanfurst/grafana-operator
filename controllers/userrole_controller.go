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
	"github.com/grafana/grafana-openapi-client-go/client/users"
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

// GrafanaUserRoleReconciler reconciles a GrafanaUserRole object
type GrafanaUserRoleReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Log    logr.Logger
}

//+kubebuilder:rbac:groups=grafana.integreatly.org,resources=grafanauserroles,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=grafana.integreatly.org,resources=grafanauserroles/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=grafana.integreatly.org,resources=grafanauserroles/finalizers,verbs=update

func (r *GrafanaUserRoleReconciler) syncUserRoles(ctx context.Context) (ctrl.Result, error) {
	syncLog := log.FromContext(ctx).WithName("GrafanaUserRoleReconciler")
	syncLog.Info("syncUserRoles")
	userrolessSynced := 0

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

	// get all userroless
	allUserRoles := &v1beta1.GrafanaUserRoleList{}
	err = r.Client.List(ctx, allUserRoles, opts...)
	if err != nil {
		return ctrl.Result{
			Requeue: true,
		}, err
	}

	// sync userroless, delete userroless from grafana that do no longer have a cr
	userrolessToDelete := map[*v1beta1.Grafana][]v1beta1.NamespacedResource{}
	for _, grafana := range grafanas.Items {
		grafana := grafana

		for _, userrole := range grafana.Status.UserRoles {
			if allUserRoles.Find(userrole.Namespace(), userrole.Name()) == nil {
				userrolessToDelete[&grafana] = append(userrolessToDelete[&grafana], userrole)
			}
		}
	}

	// delete all useroless that no longer have a cr
	for grafana, existingUserRoless := range userrolessToDelete {
		grafana := grafana
		grafanaClient, err := client2.NewGeneratedGrafanaClient(ctx, r.Client, grafana)
		if err != nil {
			return ctrl.Result{Requeue: true}, err
		}

		for _, userrole := range existingUserRoless {
			// avoid bombarding the grafana instance with a large number of requests at once, limit
			// the sync to ten users per cycle. This means that it will take longer to sync
			// a large number of deleted user crs, but that should be an edge case.
			if userrolessSynced >= syncBatchSize {
				return ctrl.Result{Requeue: true}, nil
			}

			namespace, name, id := userrole.Split()
			orgUserRoleNames := strings.Split(id, "~")
			org, err := grafanaClient.Orgs.GetOrgByName(orgUserRoleNames[0])

			if orgUserRoleNames != nil && len(orgUserRoleNames) > 2 {
				if err != nil {
					var notFound *orgs.GetOrgByNameInternalServerError
					if errors.As(err, &notFound) {
						syncLog.Info(fmt.Sprintf("organization no longer exists", orgUserRoleNames[1]), "namespace", namespace, "name", name)
					}
				}

				user, err := grafanaClient.Users.GetUserByLoginOrEmail(orgUserRoleNames[1])
				if err != nil {
					var notFound *users.GetUserByLoginOrEmailNotFound
					if errors.As(err, &notFound) {
						syncLog.Info(fmt.Sprintf("user no longer exists", orgUserRoleNames[1]), "namespace", namespace, "name", name)
					}
				}

				if org != nil && user != nil {
					instanceUsers, err := grafanaClient.Orgs.GetOrgUsers(org.Payload.ID)
					if err != nil {
						var notFound *orgs.GetOrgUsersInternalServerError
						if errors.As(err, &notFound) {
							return ctrl.Result{Requeue: false}, err
						}
						syncLog.Info(fmt.Sprintf("cannot get users for organization %s : %s"), "namespace", namespace, "name", name)
					} else if instanceUsers != nil {
						for _, orgUser := range instanceUsers.Payload {
							if orgUser.UserID == user.Payload.ID {

								_, err = grafanaClient.Orgs.RemoveOrgUser(user.Payload.ID, org.Payload.ID) //nolint
								if err != nil {
									var notFound *orgs.RemoveOrgUserInternalServerError
									if errors.As(err, &notFound) {
										return ctrl.Result{Requeue: false}, err
									}
								}
							}
						}
					}
				}
			}

			grafana.Status.UserRoles = grafana.Status.UserRoles.Remove(namespace, name)
			userrolessSynced += 1
		}

		// one update per grafana - this will trigger a reconcile of the grafana controller
		// so we should minimize those updates
		err = r.Client.Status().Update(ctx, grafana)
		if err != nil {
			return ctrl.Result{Requeue: false}, err
		}

	}

	if userrolessSynced > 0 {
		syncLog.Info("successfully synced users", "users", userrolessSynced)
	}

	return ctrl.Result{}, nil

}

func (r *GrafanaUserRoleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	controllerLog := log.FromContext(ctx).WithName("GrafanaUserRoleReconciler")
	r.Log = controllerLog

	// periodic sync reconcile
	if req.Namespace == "" && req.Name == "" {
		start := time.Now()
		syncResult, err := r.syncUserRoles(ctx)
		elapsed := time.Since(start).Milliseconds()
		metrics.InitialUserRolesSyncDuration.Set(float64(elapsed))
		return syncResult, err
	}

	cr := &v1beta1.GrafanaUserRole{}
	err := r.Client.Get(ctx, client.ObjectKey{
		Namespace: req.Namespace,
		Name:      req.Name,
	}, cr)
	if err != nil {
		if kuberr.IsNotFound(err) {
			err = r.onUserRoleDeleted(ctx, req.Namespace, req.Name)
			if err != nil {
				return ctrl.Result{RequeueAfter: RequeueDelay}, err
			}
			return ctrl.Result{}, nil
		}
		controllerLog.Error(err, "error getting grafana userrole cr")
		return ctrl.Result{RequeueAfter: RequeueDelay}, err
	}

	if cr.Spec.UserRole == nil {
		controllerLog.Info("skipped userrole with empty spec", cr.Name, cr.Namespace)
		// TODO: add a custom status around that?
		return ctrl.Result{}, nil
	}

	instances, err := r.GetMatchingUserRoleInstances(ctx, cr, r.Client)
	if err != nil {
		controllerLog.Error(err, "could not find matching instances", "name", cr.Name, "namespace", cr.Namespace)
		return ctrl.Result{RequeueAfter: RequeueDelay}, err
	}

	controllerLog.Info("found matching Grafana instances for userrole", "count", len(instances.Items))

	userrole, hash, err := r.getUserRoleContent(ctx, cr)
	if err != nil {
		controllerLog.Error(err, "could not retrieve userrole contents", "name", cr.Name, "namespace", cr.Namespace)
		return ctrl.Result{RequeueAfter: RequeueDelay}, err
	}

	if cr.IsUpdatedUID(fmt.Sprintf("%s~%s~%s", cr.Spec.UserRole.OrgName, cr.Spec.UserRole.LoginOrEmail, userrole.Role)) {
		controllerLog.Info("userrole name got updated, deleting userrole with the old name")
		err = r.onUserRoleDeleted(ctx, req.Namespace, req.Name)
		if err != nil {
			return ctrl.Result{RequeueAfter: RequeueDelay}, err
		}

		// Clean up uid, so further reconcilications can track changes there
		cr.Status.UID = ""

		err = r.Client.Status().Update(ctx, cr)
		if err != nil {
			return ctrl.Result{RequeueAfter: RequeueDelay}, err
		}

		// Status update should trigger the next reconciliation right away, no need to requeue for dashboard creation
		return ctrl.Result{}, nil
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

		// then import the userrole into the matching grafana instances
		err = r.onUserRoleCreated(ctx, &grafana, cr, userrole, hash)
		if err != nil {
			success = false
			cr.Status.LastMessage = err.Error()
			controllerLog.Error(err, "error reconciling userrole", "userrole", cr.Name, "grafana", grafana.Name)
		}
	}

	// if the userrole was successfully synced in all instances, wait for its re-sync period
	if success {
		cr.Status.LastMessage = ""
		cr.Status.Hash = hash
		if cr.ResyncPeriodHasElapsed() {
			cr.Status.LastResync = metav1.Time{Time: time.Now()}
		}
		cr.Status.UID = fmt.Sprintf("%s~%s~%s", cr.Spec.UserRole.OrgName, cr.Spec.UserRole.LoginOrEmail, cr.Spec.UserRole.Role)
		return ctrl.Result{RequeueAfter: cr.GetResyncPeriod()}, r.Client.Status().Update(ctx, cr)
	} else {
		// if there was an issue with the userrole, update the status
		return ctrl.Result{RequeueAfter: RequeueDelay}, r.Client.Status().Update(ctx, cr)
	}

}

func (r *GrafanaUserRoleReconciler) onUserRoleDeleted(ctx context.Context, namespace string, name string) error {
	log := log.FromContext(ctx).WithName("GrafanaUserRoleReconciler")
	log.Info("onUserRoleDeleted")

	list := v1beta1.GrafanaList{}
	opts := []client.ListOption{}
	err := r.Client.List(ctx, &list, opts...)
	if err != nil {
		return err
	}

	for _, grafana := range list.Items {
		grafana := grafana
		if found, userRoleId := grafana.Status.UserRoles.Find(namespace, name); found {
			grafanaClient, err := client2.NewGeneratedGrafanaClient(ctx, r.Client, &grafana)
			if err != nil {
				return err
			}

			orgUserRoleName := strings.Split(*userRoleId, "~")

			getOrgResponse, err := grafanaClient.Orgs.GetOrgByName(orgUserRoleName[0])
			if err != nil {
				var notFound *orgs.GetOrgByNameInternalServerError
				if errors.As(err, &notFound) {
					return err
				}
			}

			getUserResponse, err := grafanaClient.Users.GetUserByLoginOrEmail(orgUserRoleName[1])
			if err != nil {
				var notFound *orgs.GetOrgByNameInternalServerError
				if errors.As(err, &notFound) {
					return err
				}
			}

			if getOrgResponse != nil && getUserResponse != nil {
				_, err = grafanaClient.Orgs.RemoveOrgUser(getUserResponse.Payload.ID, getOrgResponse.Payload.ID) //nolint
				if err != nil {
					var notFound *orgs.RemoveOrgUserInternalServerError
					if errors.As(err, &notFound) {
						return err
					}
				}
			}

			grafana.Status.UserRoles = grafana.Status.UserRoles.Remove(namespace, name)
			return r.Client.Status().Update(ctx, &grafana)
		}
	}

	return nil

}

func (r *GrafanaUserRoleReconciler) onUserRoleCreated(ctx context.Context, grafana *v1beta1.Grafana, cr *v1beta1.GrafanaUserRole, userrole *models.UpdateOrgUserCommand, hash string) error {

	logger := log.FromContext(ctx).WithName("onUserRoleCreated")

	if cr.Spec.UserRole == nil {
		return nil
	}

	grafanaClient, err := client2.NewGeneratedGrafanaClient(ctx, r.Client, grafana)
	if err != nil {
		return err
	}

	exists, orgId, orgUserId, err := r.Exists(grafanaClient, cr)
	if err != nil {
		return err
	}

	if exists && cr.Unchanged(hash) && !cr.ResyncPeriodHasElapsed() {
		return nil
	}

	encoded, err := json.Marshal(userrole)
	if err != nil {
		return fmt.Errorf("representing userrole as JSON: %w", err)
	}

	if exists {
		//var body models.UpdateOrg //models.UpdateOrgUserCommand
		var body orgs.UpdateOrgUserParams

		if err := json.Unmarshal(encoded, &body.Body); err != nil {
			return fmt.Errorf("representing userrole as update command: %w", err)
		}
		body.UserID = orgUserId
		body.OrgID = orgId
		logger.Info(fmt.Sprintf("UpdateOrgUserParams: %s", body))
		//userrole.UID = uid
		_, err := grafanaClient.Orgs.UpdateOrgUser(&body)
		//_, err := grafanaClient.Datasources.UpdateDataSourceByUID(userrole.UID, &body) //nolint
		if err != nil {
			return err
		}
	} else {
		var body models.AddOrgUserCommand

		if err := json.Unmarshal(encoded, &body); err != nil {
			return fmt.Errorf("representing userrole as create command: %w", err)
		}
		body.LoginOrEmail = cr.Spec.UserRole.LoginOrEmail
		logger.Info(fmt.Sprintf("AddOrgUserCommand: %s", body))
		_, err = grafanaClient.Orgs.AddOrgUser(orgId, &body) //nolint
		if err != nil {
			return err
		}
	}

	grafana.Status.UserRoles = grafana.Status.UserRoles.Add(cr.Namespace, cr.Name, fmt.Sprintf("%s~%s~%s", cr.Spec.UserRole.OrgName, cr.Spec.UserRole.LoginOrEmail, cr.Spec.UserRole.Role))
	return r.Client.Status().Update(ctx, grafana)

}

func (r *GrafanaUserRoleReconciler) Exists(client *genapi.GrafanaHTTPAPI, cr *v1beta1.GrafanaUserRole) (bool, int64, int64, error) {

	organizationExists, orgId, err := orgExists(client, cr.Spec.UserRole.OrgName)
	if err != nil {
		return false, 0, 0, err
	}

	if !organizationExists {
		return false, 0, 0, fmt.Errorf("%s userrole not created because %s organization not exists", cr.Name, cr.Spec.UserRole.OrgName)
	}

	grafanaUserExists, _, err := userExists(client, cr.Spec.UserRole.LoginOrEmail)
	if err != nil {
		return false, 0, 0, err
	}
	if !grafanaUserExists {
		return false, 0, 0, fmt.Errorf("%s userrole not created because %s user not exists", cr.Name, cr.Spec.UserRole.LoginOrEmail)
	}

	organizationUsers, err := client.Orgs.GetOrgUsers(orgId)
	if err != nil {
		return false, 0, 0, fmt.Errorf("fetching orgUsers: %w", err)
	}

	if organizationUsers != nil {
		for _, orgUser := range organizationUsers.Payload {
			if orgUser.Login == cr.Spec.UserRole.LoginOrEmail || orgUser.Email == cr.Spec.UserRole.LoginOrEmail {
				return true, orgId, orgUser.UserID, nil
			}
		}
	}
	return false, orgId, 0, nil

}

func orgExists(client *genapi.GrafanaHTTPAPI, name string) (bool, int64, error) {
	org, err := client.Orgs.GetOrgByName(name)
	if err != nil {
		return false, 0, fmt.Errorf("fetching org for userrole: %w", err)
	}

	if org != nil {
		return true, org.Payload.ID, nil
	}

	return false, 0, nil

}

func userExists(client *genapi.GrafanaHTTPAPI, name string) (bool, int64, error) {
	user, err := client.Users.GetUserByLoginOrEmail(name)
	if err != nil && !strings.Contains(err.Error(), "\"user not found\"") {
		return false, 0, fmt.Errorf("fetching user: %w", err)
	}

	if user != nil {
		return true, user.Payload.ID, nil
	}

	return false, 0, nil

}

func (r *GrafanaUserRoleReconciler) SetupWithManager(mgr ctrl.Manager, ctx context.Context) error {
	err := ctrl.NewControllerManagedBy(mgr).
		For(&v1beta1.GrafanaUserRole{}).
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
						r.Log.Error(err, "error synchronizing userroles")
						continue
					}
					if result.Requeue {
						r.Log.Info("more userroles left to synchronize")
						continue
					}
					r.Log.Info("userroles sync complete")
					return
				}
			}
		}()
	}
	return err
}

func (r *GrafanaUserRoleReconciler) GetMatchingUserRoleInstances(ctx context.Context, userrole *v1beta1.GrafanaUserRole, k8sClient client.Client) (v1beta1.GrafanaList, error) {
	instances, err := GetMatchingInstances(ctx, k8sClient, userrole.Spec.InstanceSelector)
	if err != nil || len(instances.Items) == 0 {
		userrole.Status.NoMatchingInstances = true
		if err := r.Client.Status().Update(ctx, userrole); err != nil {
			r.Log.Info("unable to update the status of %v, in %v", userrole.Name, userrole.Namespace)
		}
		return v1beta1.GrafanaList{}, err
	}
	userrole.Status.NoMatchingInstances = false
	if err := r.Client.Status().Update(ctx, userrole); err != nil {
		r.Log.Info("unable to update the status of %v, in %v", userrole.Name, userrole.Namespace)
	}

	return instances, err
}

func (r *GrafanaUserRoleReconciler) getUserRoleContent(ctx context.Context, cr *v1beta1.GrafanaUserRole) (*models.UpdateOrgUserCommand, string, error) {
	initialBytes, err := json.Marshal(cr.Spec.UserRole)
	if err != nil {
		return nil, "", err
	}

	simpleContent, err := simplejson.NewJson(initialBytes)
	if err != nil {
		return nil, "", err
	}

	/*if cr.Spec.UserRole.UID == "" {
		simpleContent.Set("uid", string(cr.UID))
	}*/

	newBytes, err := simpleContent.MarshalJSON()
	if err != nil {
		return nil, "", err
	}

	// We use UpdateOrgForm here because models.DataSource lacks the SecureJsonData field
	var res models.UpdateOrgUserCommand
	if err = json.Unmarshal(newBytes, &res); err != nil {
		return nil, "", err
	}

	hash := sha256.New()
	hash.Write(newBytes)

	return &res, fmt.Sprintf("%x", hash.Sum(nil)), nil
}
