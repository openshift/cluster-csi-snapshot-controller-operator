package operator

import (
	"errors"
	"fmt"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	corev1 "k8s.io/api/core/v1"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apiextinformersv1 "k8s.io/apiextensions-apiserver/pkg/client/informers/externalversions/apiextensions/v1"
	apiextlistersv1 "k8s.io/apiextensions-apiserver/pkg/client/listers/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	appsinformersv1 "k8s.io/client-go/informers/apps/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/status"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	logf "sigs.k8s.io/controller-runtime/pkg/runtime/log"
)

var log = logf.Log.WithName("csi_snapshot_controller_operator")
var deploymentVersionHashKey = operatorv1.GroupName + "/rvs-hash"

const (
	targetName        = "csi-snapshot-controller"
	targetNamespace   = "openshift-cluster-storage-operator"
	operatorNamespace = "openshift-cluster-storage-operator"
	globalConfigName  = "cluster"

	operatorVersionEnvName = "OPERATOR_IMAGE_VERSION"
	operandVersionEnvName  = "OPERAND_IMAGE_VERSION"
	operandImageEnvName    = "OPERAND_IMAGE"

	maxRetries = 15
)

// static environment variables from operator deployment
var (
	crdNames = []string{"volumesnapshotclasses.snapshot.storage.k8s.io", "volumesnapshotcontents.snapshot.storage.k8s.io", "volumesnapshots.snapshot.storage.k8s.io"}
)

type csiSnapshotOperator struct {
	client        OperatorClient
	kubeClient    kubernetes.Interface
	versionGetter status.VersionGetter
	eventRecorder events.Recorder

	syncHandler func() error

	crdLister       apiextlistersv1.CustomResourceDefinitionLister
	crdListerSynced cache.InformerSynced
	crdClient       apiextclient.Interface

	queue workqueue.RateLimitingInterface

	stopCh <-chan struct{}

	operatorVersion            string
	operandVersion             string
	csiSnapshotControllerImage string
}

func NewCSISnapshotControllerOperator(
	client OperatorClient,
	crdInformer apiextinformersv1.CustomResourceDefinitionInformer,
	crdClient apiextclient.Interface,
	deployInformer appsinformersv1.DeploymentInformer,
	kubeClient kubernetes.Interface,
	versionGetter status.VersionGetter,
	eventRecorder events.Recorder,
	operatorVersion string,
	operandVersion string,
	csiSnapshotControllerImage string,
) *csiSnapshotOperator {
	csiOperator := &csiSnapshotOperator{
		client:                     client,
		crdClient:                  crdClient,
		kubeClient:                 kubeClient,
		versionGetter:              versionGetter,
		eventRecorder:              eventRecorder,
		queue:                      workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "csi-snapshot-controller"),
		operatorVersion:            operatorVersion,
		operandVersion:             operandVersion,
		csiSnapshotControllerImage: csiSnapshotControllerImage,
	}

	crdInformer.Informer().AddEventHandler(csiOperator.eventHandler("crd"))
	deployInformer.Informer().AddEventHandler(csiOperator.eventHandler("deployment"))
	client.Informer().AddEventHandler(csiOperator.eventHandler("csisnapshotcontroller"))

	csiOperator.syncHandler = csiOperator.sync

	csiOperator.crdLister = crdInformer.Lister()
	csiOperator.crdListerSynced = crdInformer.Informer().HasSynced

	return csiOperator
}

func (c *csiSnapshotOperator) Run(workers int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	c.stopCh = stopCh

	if !cache.WaitForCacheSync(stopCh, c.crdListerSynced, c.client.Informer().HasSynced) {
		return
	}

	for i := 0; i < workers; i++ {
		go wait.Until(c.worker, time.Second, stopCh)
	}
	<-stopCh
}

func (c *csiSnapshotOperator) sync() error {
	instance, err := c.client.GetOperatorInstance()
	if err != nil {
		return err
	}

	if instance.Spec.ManagementState != operatorv1.Managed {
		return nil // TODO do something better for all states
	}

	instanceCopy := instance.DeepCopy()

	// Watch CRs for:
	// - CSISnapshots
	// - Deployments
	// - CRD
	// - Status?

	// Ensure the CSISnapshotController deployment exists and matches the default
	// If it doesn't exist, create it.
	// If it does exist and doesn't match, overwrite it
	startTime := time.Now()
	klog.Info("Starting syncing operator at ", startTime)
	defer func() {
		klog.Info("Finished syncing operator at ", time.Since(startTime))
	}()

	syncErr := c.handleSync(instanceCopy)
	c.updateSyncError(&instanceCopy.Status.OperatorStatus, syncErr)

	if _, _, err := v1helpers.UpdateStatus(c.client, func(status *operatorv1.OperatorStatus) error {
		// store a copy of our starting conditions, we need to preserve last transition time
		originalConditions := status.DeepCopy().Conditions

		// copy over everything else
		instanceCopy.Status.OperatorStatus.DeepCopyInto(status)

		// restore the starting conditions
		status.Conditions = originalConditions

		// manually update the conditions while preserving last transition time
		for _, condition := range instanceCopy.Status.Conditions {
			v1helpers.SetOperatorCondition(&status.Conditions, condition)
		}
		return nil
	}); err != nil {
		klog.Errorf("failed to update status: %v", err)
		if syncErr == nil {
			syncErr = err
		}
	}

	return syncErr
}

func (c *csiSnapshotOperator) updateSyncError(status *operatorv1.OperatorStatus, err error) {
	if err != nil {
		degradedReason := "OperatorSync"
		var errAlpha *AlphaCRDError
		if errors.As(err, &errAlpha) {
			degradedReason = "AlphaCRDsExist"
		}
		v1helpers.SetOperatorCondition(&status.Conditions,
			operatorv1.OperatorCondition{
				Type:    operatorv1.OperatorStatusTypeDegraded,
				Status:  operatorv1.ConditionTrue,
				Reason:  degradedReason,
				Message: err.Error(),
			})
	} else {
		v1helpers.SetOperatorCondition(&status.Conditions,
			operatorv1.OperatorCondition{
				Type:   operatorv1.OperatorStatusTypeDegraded,
				Status: operatorv1.ConditionFalse,
			})
	}
}

func (c *csiSnapshotOperator) handleSync(instance *operatorv1.CSISnapshotController) error {
	if err := c.syncCustomResourceDefinitions(); err != nil {
		// Pass through AlphaCRDError via %w
		return fmt.Errorf("failed to sync CRDs: %w", err)
	}

	deployment, err := c.syncDeployment(instance)
	if err != nil {
		return fmt.Errorf("failed to sync Deployments: %s", err)
	}
	if err := c.syncStatus(instance, deployment); err != nil {
		return fmt.Errorf("failed to sync status: %s", err)
	}
	return nil
}

func (c *csiSnapshotOperator) setVersion(operandName, version string) {
	if c.versionGetter.GetVersions()[operandName] != version {
		c.versionGetter.SetVersion(operandName, version)
	}
}

func (c *csiSnapshotOperator) versionChanged(operandName, version string) bool {
	return c.versionGetter.GetVersions()[operandName] != version
}

func (c *csiSnapshotOperator) enqueue(obj interface{}) {
	// we're filtering out config maps that are "leader" based and we don't have logic around them
	// resyncing on these causes the operator to sync every 14s for no good reason
	if cm, ok := obj.(*corev1.ConfigMap); ok && cm.GetAnnotations() != nil && cm.GetAnnotations()[resourcelock.LeaderElectionRecordAnnotationKey] != "" {
		return
	}
	// Sync corresponding CSISnapshotController instance. Since there is only one, sync that one.
	// It will check all other objects (CRDs, Deployment) and update/overwrite them as needed.
	c.queue.Add(globalConfigName)
}

func (c *csiSnapshotOperator) eventHandler(kind string) cache.ResourceEventHandler {
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			logInformerEvent(kind, obj, "added")
			c.enqueue(obj)
		},
		UpdateFunc: func(old, new interface{}) {
			logInformerEvent(kind, new, "updated")
			c.enqueue(new)
		},
		DeleteFunc: func(obj interface{}) {
			logInformerEvent(kind, obj, "deleted")
			c.enqueue(obj)
		},
	}
}

func logInformerEvent(kind, obj interface{}, message string) {
	if klog.V(6).Enabled() {
		objMeta, err := meta.Accessor(obj)
		if err != nil {
			return
		}
		// "deployment csi-snapshot-controller updated"
		klog.V(6).Infof("Received event: %s %s %s", kind, objMeta.GetName(), message)
	}
}

func (c *csiSnapshotOperator) worker() {
	for c.processNextWorkItem() {
	}
}

func (c *csiSnapshotOperator) processNextWorkItem() bool {
	key, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(key)

	err := c.syncHandler()
	c.handleErr(err, key)

	return true
}

func (c *csiSnapshotOperator) handleErr(err error, key interface{}) {
	if err == nil {
		c.queue.Forget(key)
		return
	}

	if c.queue.NumRequeues(key) < maxRetries {
		klog.V(2).Infof("Error syncing operator %v: %v", key, err)
		c.queue.AddRateLimited(key)
		return
	}

	utilruntime.HandleError(err)
	klog.V(2).Infof("Dropping operator %q out of the queue: %v", key, err)
	c.queue.Forget(key)
	c.queue.AddAfter(key, 1*time.Minute)
}
