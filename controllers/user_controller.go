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
	"github.com/grafana/grafana-openapi-client-go/client/admin_users"
	"github.com/grafana/grafana-openapi-client-go/client/users"
	"github.com/grafana/grafana-openapi-client-go/models"
	"github.com/grafana/grafana-operator/v5/api/v1beta1"
	client2 "github.com/grafana/grafana-operator/v5/controllers/client"
	"github.com/grafana/grafana-operator/v5/controllers/metrics"
	kuberr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"math/rand"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"strings"
	"time"
)

const passwordCharset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// GrafanaUserReconciler reconciles a GrafanaUser object
type GrafanaUserReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Log    logr.Logger
}

//+kubebuilder:rbac:groups=grafana.integreatly.org,resources=grafanausers,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=grafana.integreatly.org,resources=grafanausers/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=grafana.integreatly.org,resources=grafanausers/finalizers,verbs=update

func (r *GrafanaUserReconciler) syncUsers(ctx context.Context) (ctrl.Result, error) {
	syncLog := log.FromContext(ctx).WithName("GrafanaUserReconciler")
	syncLog.Info("syncUsers")
	usersSynced := 0

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

	// get all users
	allUsers := &v1beta1.GrafanaUserList{}
	err = r.Client.List(ctx, allUsers, opts...)
	if err != nil {
		return ctrl.Result{
			Requeue: true,
		}, err
	}

	// sync users, delete users from grafana that do no longer have a cr
	usersToDelete := map[*v1beta1.Grafana][]v1beta1.NamespacedResource{}
	for _, grafana := range grafanas.Items {
		grafana := grafana

		for _, user := range grafana.Status.Users {
			if allUsers.Find(user.Namespace(), user.Name()) == nil {
				usersToDelete[&grafana] = append(usersToDelete[&grafana], user)
			}
		}
	}

	// delete all users that no longer have a cr
	for grafana, existingUsers := range usersToDelete {
		grafana := grafana
		grafanaClient, err := client2.NewGeneratedGrafanaClient(ctx, r.Client, grafana)
		if err != nil {
			return ctrl.Result{Requeue: true}, err
		}

		for _, user := range existingUsers {
			// avoid bombarding the grafana instance with a large number of requests at once, limit
			// the sync to ten users per cycle. This means that it will take longer to sync
			// a large number of deleted user crs, but that should be an edge case.
			if usersSynced >= syncBatchSize {
				return ctrl.Result{Requeue: true}, nil
			}

			namespace, name, loginOrEmail := user.Split()
			instanceUser, err := grafanaClient.Users.GetUserByLoginOrEmail(loginOrEmail)
			if err != nil {
				var notFound *users.GetUserByLoginOrEmailNotFound
				if errors.As(err, &notFound) {
					return ctrl.Result{Requeue: false}, err
				}
				syncLog.Info("user no longer exists", "namespace", namespace, "name", name)
			} else {
				_, err = grafanaClient.AdminUsers.AdminDeleteUser(instanceUser.Payload.ID) //nolint
				if err != nil {
					var notFound *admin_users.AdminDeleteUserNotFound
					if errors.As(err, &notFound) {
						return ctrl.Result{Requeue: false}, err
					}
				}
			}

			grafana.Status.Users = grafana.Status.Users.Remove(namespace, name)
			usersSynced += 1

		}

		// one update per grafana - this will trigger a reconcile of the grafana controller
		// so we should minimize those updates
		err = r.Client.Status().Update(ctx, grafana)
		if err != nil {
			return ctrl.Result{Requeue: false}, err
		}

	}

	if usersSynced > 0 {
		syncLog.Info("successfully synced users", "users", usersSynced)
	}

	return ctrl.Result{}, nil

}

func (r *GrafanaUserReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	controllerLog := log.FromContext(ctx).WithName("GrafanaUserReconciler")
	r.Log = controllerLog

	// periodic sync reconcile
	if req.Namespace == "" && req.Name == "" {
		start := time.Now()
		syncResult, err := r.syncUsers(ctx)
		elapsed := time.Since(start).Milliseconds()
		metrics.InitialUsersSyncDuration.Set(float64(elapsed))
		return syncResult, err
	}

	cr := &v1beta1.GrafanaUser{}
	err := r.Client.Get(ctx, client.ObjectKey{
		Namespace: req.Namespace,
		Name:      req.Name,
	}, cr)
	if err != nil {
		if kuberr.IsNotFound(err) {
			err = r.onUserDeleted(ctx, req.Namespace, req.Name)
			if err != nil {
				return ctrl.Result{RequeueAfter: RequeueDelay}, err
			}
			return ctrl.Result{}, nil
		}
		controllerLog.Error(err, "error getting grafana datasource cr")
		return ctrl.Result{RequeueAfter: RequeueDelay}, err
	}

	if cr.Spec.User == nil {
		controllerLog.Info("skipped datasource with empty spec", cr.Name, cr.Namespace)
		// TODO: add a custom status around that?
		return ctrl.Result{}, nil
	}

	instances, err := r.GetMatchingUserInstances(ctx, cr, r.Client)
	if err != nil {
		controllerLog.Error(err, "could not find matching instances", "name", cr.Name, "namespace", cr.Namespace)
		return ctrl.Result{RequeueAfter: RequeueDelay}, err
	}

	controllerLog.Info("found matching Grafana instances for user", "count", len(instances.Items))

	user, hash, err := r.getUserContent(ctx, cr)
	if err != nil {
		controllerLog.Error(err, "could not retrieve user contents", "name", cr.Name, "namespace", cr.Namespace)
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

		// then import the datasource into the matching grafana instances
		err = r.onUserCreated(ctx, &grafana, cr, user, hash)
		if err != nil {
			var forbidden *users.UpdateUserForbidden
			if !(errors.As(err, &forbidden) && strings.Contains(err.Error(), "User info cannot be updated for external Users")) {
				success = false
				cr.Status.LastMessage = err.Error()
				controllerLog.Error(err, "error reconciling user", "user", cr.Name, "grafana", grafana.Name)

			}
		}
	}

	// if the datasource was successfully synced in all instances, wait for its re-sync period
	if success {
		cr.Status.LastMessage = ""
		cr.Status.Hash = hash
		if cr.ResyncPeriodHasElapsed() {
			cr.Status.LastResync = metav1.Time{Time: time.Now()}
		}
		cr.Status.UID = user.Name
		return ctrl.Result{RequeueAfter: cr.GetResyncPeriod()}, r.Client.Status().Update(ctx, cr)
	} else {
		// if there was an issue with the datasource, update the status
		return ctrl.Result{RequeueAfter: RequeueDelay}, r.Client.Status().Update(ctx, cr)
	}

}

func (r *GrafanaUserReconciler) onUserDeleted(ctx context.Context, namespace string, name string) error {
	log := log.FromContext(ctx).WithName("GrafanaUserReconciler")
	log.Info("onUserDeleted")

	list := v1beta1.GrafanaList{}
	opts := []client.ListOption{}
	err := r.Client.List(ctx, &list, opts...)
	if err != nil {
		return err
	}

	for _, grafana := range list.Items {
		grafana := grafana
		if found, loginOrEmail := grafana.Status.Users.Find(namespace, name); found {
			grafanaClient, err := client2.NewGeneratedGrafanaClient(ctx, r.Client, &grafana)
			if err != nil {
				return err
			}

			userInstamce, err := grafanaClient.Users.GetUserByLoginOrEmail(*loginOrEmail)
			if err != nil {
				var notFound *users.GetUserByLoginOrEmailNotFound
				if errors.As(err, &notFound) {
					return err
				}
			} else if userInstamce != nil {
				_, err = grafanaClient.AdminUsers.AdminDeleteUser(userInstamce.Payload.ID) //nolint
				if err != nil {
					var notFound *admin_users.AdminDeleteUserNotFound
					if errors.As(err, &notFound) {
						return err
					}
				}
			}

			grafana.Status.Users = grafana.Status.Users.Remove(namespace, name)
			return r.Client.Status().Update(ctx, &grafana)
		}
	}

	return nil

}

func (r *GrafanaUserReconciler) onUserCreated(ctx context.Context, grafana *v1beta1.Grafana, cr *v1beta1.GrafanaUser, user *models.UpdateUserCommand, hash string) error {

	if cr.Spec.User == nil {
		return nil
	}

	grafanaClient, err := client2.NewGeneratedGrafanaClient(ctx, r.Client, grafana)
	if err != nil {
		return err
	}

	exists, id, err := r.Exists(grafanaClient, user.Name)
	if err != nil {
		return err
	}

	if exists && cr.Unchanged(hash) && !cr.ResyncPeriodHasElapsed() {
		return nil
	}

	encoded, err := json.Marshal(user)
	if err != nil {
		return fmt.Errorf("representing user as JSON: %w", err)
	}
	if exists {
		var body models.UpdateUserCommand
		if err := json.Unmarshal(encoded, &body); err != nil {
			return fmt.Errorf("representing user as update command: %w", err)
		}
		//user.UID = uid
		_, err := grafanaClient.Users.UpdateUser(id, &body)
		//_, err := grafanaClient.Datasources.UpdateDataSourceByUID(user.UID, &body) //nolint
		if err != nil {
			return err
		}
	} else {
		var body models.AdminCreateUserForm

		if err := json.Unmarshal(encoded, &body); err != nil {
			return fmt.Errorf("representing user as create command: %w", err)
		}

		if cr.Spec.User.Password == "" {
			body.Password = models.Password(generateRandomPassword(20))
		} else {
			body.Password = models.Password(cr.Spec.User.Password)
		}

		createUserResponse, err := grafanaClient.AdminUsers.AdminCreateUser(&body) //nolint
		if err != nil {
			return err
		}
		id = createUserResponse.Payload.ID
	}

	grafana.Status.Users = grafana.Status.Users.Add(cr.Namespace, cr.Name, user.Name)
	return r.Client.Status().Update(ctx, grafana)

}

func (r *GrafanaUserReconciler) Exists(client *genapi.GrafanaHTTPAPI, name string) (bool, int64, error) {
	user, err := client.Users.GetUserByLoginOrEmail(name)
	if err != nil && !strings.Contains(err.Error(), "\"user not found\"") {
		return false, 0, fmt.Errorf("fetching user: %w", err)
	}

	if user != nil {
		return true, user.Payload.ID, nil
	}

	return false, 0, nil

}

func (r *GrafanaUserReconciler) SetupWithManager(mgr ctrl.Manager, ctx context.Context) error {
	err := ctrl.NewControllerManagedBy(mgr).
		For(&v1beta1.GrafanaUser{}).
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
						r.Log.Error(err, "error synchronizing users")
						continue
					}
					if result.Requeue {
						r.Log.Info("more users left to synchronize")
						continue
					}
					r.Log.Info("users sync complete")
					return
				}
			}
		}()
	}
	return err
}

func (r *GrafanaUserReconciler) GetMatchingUserInstances(ctx context.Context, user *v1beta1.GrafanaUser, k8sClient client.Client) (v1beta1.GrafanaList, error) {
	instances, err := GetMatchingInstances(ctx, k8sClient, user.Spec.InstanceSelector)
	if err != nil || len(instances.Items) == 0 {
		user.Status.NoMatchingInstances = true
		if err := r.Client.Status().Update(ctx, user); err != nil {
			r.Log.Info("unable to update the status of %v, in %v", user.Name, user.Namespace)
		}
		return v1beta1.GrafanaList{}, err
	}
	user.Status.NoMatchingInstances = false
	if err := r.Client.Status().Update(ctx, user); err != nil {
		r.Log.Info("unable to update the status of %v, in %v", user.Name, user.Namespace)
	}

	return instances, err
}

func (r *GrafanaUserReconciler) getUserContent(ctx context.Context, cr *v1beta1.GrafanaUser) (*models.UpdateUserCommand, string, error) {
	initialBytes, err := json.Marshal(cr.Spec.User)
	if err != nil {
		return nil, "", err
	}

	simpleContent, err := simplejson.NewJson(initialBytes)
	if err != nil {
		return nil, "", err
	}

	/*if cr.Spec.User.UID == "" {
		simpleContent.Set("uid", string(cr.UID))
	}*/

	newBytes, err := simpleContent.MarshalJSON()
	if err != nil {
		return nil, "", err
	}

	// We use UpdateUserCommand here because models.DataSource lacks the SecureJsonData field
	var res models.UpdateUserCommand
	if err = json.Unmarshal(newBytes, &res); err != nil {
		return nil, "", err
	}

	hash := sha256.New()
	hash.Write(newBytes)

	return &res, fmt.Sprintf("%x", hash.Sum(nil)), nil
}

func generateRandomPassword(length int) string {
	b := make([]byte, length)
	for i := range b {
		b[i] = passwordCharset[rand.New(rand.NewSource(time.Now().UnixNano())).Intn(len(passwordCharset))]
	}
	return string(b)
}
