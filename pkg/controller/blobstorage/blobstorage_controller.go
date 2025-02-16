package blobstorage

import (
	"context"
	"fmt"

	"github.com/integr8ly/cloud-resource-operator/pkg/apis/integreatly/v1alpha1/types"
	"github.com/integr8ly/cloud-resource-operator/pkg/providers/openshift"
	"github.com/integr8ly/cloud-resource-operator/pkg/resources"

	"github.com/sirupsen/logrus"

	"github.com/integr8ly/cloud-resource-operator/pkg/providers/aws"

	"github.com/integr8ly/cloud-resource-operator/pkg/providers"

	"github.com/integr8ly/cloud-resource-operator/pkg/apis/integreatly/v1alpha1"
	errorUtil "github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

var log = logf.Log.WithName("controller_blobstorage")

// Add creates a new BlobStorage Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	return add(mgr, newReconciler(mgr))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) reconcile.Reconciler {
	client := mgr.GetClient()
	logger := logrus.WithFields(logrus.Fields{"controller": "controller_blobstorage"})
	providerList := []providers.BlobStorageProvider{aws.NewAWSBlobStorageProvider(client, logger), openshift.NewBlobStorageProvider(client, logger)}
	rp := resources.NewResourceProvider(client, mgr.GetScheme(), logger)
	return &ReconcileBlobStorage{
		client:           client,
		scheme:           mgr.GetScheme(),
		logger:           logger,
		resourceProvider: rp,
		providerList:     providerList,
	}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New("blobstorage-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to primary resource BlobStorage
	err = c.Watch(&source.Kind{Type: &v1alpha1.BlobStorage{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}
	return nil
}

// blank assignment to verify that ReconcileBlobStorage implements reconcile.Reconciler
var _ reconcile.Reconciler = &ReconcileBlobStorage{}

// ReconcileBlobStorage reconciles a BlobStorage object
type ReconcileBlobStorage struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client           client.Client
	scheme           *runtime.Scheme
	logger           *logrus.Entry
	resourceProvider *resources.ReconcileResourceProvider
	providerList     []providers.BlobStorageProvider
}

func (r *ReconcileBlobStorage) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	r.logger.Info("Reconciling BlobStorage")
	ctx := context.TODO()
	cfgMgr := providers.NewConfigManager(providers.DefaultProviderConfigMapName, request.Namespace, r.client)

	// Fetch the BlobStorage instance
	instance := &v1alpha1.BlobStorage{}
	err := r.client.Get(ctx, request.NamespacedName, instance)
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}

	stratMap, err := cfgMgr.GetStrategyMappingForDeploymentType(ctx, instance.Spec.Type)
	if err != nil {
		if updateErr := resources.UpdatePhase(ctx, r.client, instance, types.PhaseFailed, types.StatusDeploymentConfigNotFound.WrapError(err)); updateErr != nil {
			return reconcile.Result{}, updateErr
		}
		return reconcile.Result{}, err
	}

	for _, p := range r.providerList {
		if !p.SupportsStrategy(stratMap.BlobStorage) {
			continue
		}

		if instance.GetDeletionTimestamp() != nil {
			msg, err := p.DeleteStorage(ctx, instance)
			if err != nil {
				if updateErr := resources.UpdatePhase(ctx, r.client, instance, types.PhaseFailed, msg.WrapError(err)); updateErr != nil {
					return reconcile.Result{}, updateErr
				}
				return reconcile.Result{}, errorUtil.Wrapf(err, "failed to perform provider-specific storage deletion")
			}

			r.logger.Info("Waiting on blob storage to successfully delete")
			if err = resources.UpdatePhase(ctx, r.client, instance, types.PhaseDeleteInProgress, msg.WrapError(err)); err != nil {
				return reconcile.Result{}, err
			}
			return reconcile.Result{Requeue: true, RequeueAfter: p.GetReconcileTime(instance)}, nil
		}

		bsi, msg, err := p.CreateStorage(ctx, instance)
		if err != nil {
			instance.Status.SecretRef = &types.SecretRef{}
			if updateErr := resources.UpdatePhase(ctx, r.client, instance, types.PhaseFailed, msg.WrapError(err)); updateErr != nil {
				return reconcile.Result{}, updateErr
			}
			return reconcile.Result{}, err
		}
		if bsi == nil {
			r.logger.Info("Secret data is still reconciling, blob storage is nil")
			instance.Status.SecretRef = &types.SecretRef{}
			if err = resources.UpdatePhase(ctx, r.client, instance, types.PhaseInProgress, msg); err != nil {
				return reconcile.Result{}, err
			}
			return reconcile.Result{Requeue: true, RequeueAfter: p.GetReconcileTime(instance)}, nil
		}

		if err := r.resourceProvider.ReconcileResultSecret(ctx, instance, bsi.DeploymentDetails.Data()); err != nil {
			return reconcile.Result{}, errorUtil.Wrap(err, "failed to reconcile secret")
		}

		instance.Status.Phase = types.PhaseComplete
		instance.Status.Message = msg
		instance.Status.SecretRef = instance.Spec.SecretRef
		instance.Status.Strategy = stratMap.BlobStorage
		instance.Status.Provider = p.GetName()
		if err = r.client.Status().Update(ctx, instance); err != nil {
			return reconcile.Result{}, errorUtil.Wrapf(err, "failed to update instance %s in namespace %s", instance.Name, instance.Namespace)
		}
		return reconcile.Result{Requeue: true, RequeueAfter: p.GetReconcileTime(instance)}, nil
	}

	// unsupported strategy
	if err = resources.UpdatePhase(ctx, r.client, instance, types.PhaseFailed, types.StatusUnsupportedType.WrapError(err)); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{}, errorUtil.New(fmt.Sprintf("unsupported deployment strategy %s", stratMap.BlobStorage))
}
