package operator

import (
	"fmt"
	"os"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	corev1 "k8s.io/api/core/v1"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apiextinformersv1beta1 "k8s.io/apiextensions-apiserver/pkg/client/informers/externalversions/apiextensions/v1beta1"
	apiextlistersv1beta1 "k8s.io/apiextensions-apiserver/pkg/client/listers/apiextensions/v1beta1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"

	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/status"

	logf "sigs.k8s.io/controller-runtime/pkg/runtime/log"
)

var log = logf.Log.WithName("csi_snapshot_controller_operator")
var deploymentVersionHashKey = operatorv1.GroupName + "/rvs-hash"

const (
	targetName                = "csi-snapshot-controller"
	targetNamespace           = "openshift-csi-snapshot-controller"
	targetNameSpaceController = "openshift-csi-controller"
	targetNameOperator        = "openshift-csi-snapshot-controller-operator"
	targetNameController      = "openshift-csi-snapshot-controller"
	globalConfigName          = "cluster"

	operatorSelfName       = "operator"
	operatorVersionEnvName = "OPERATOR_IMAGE_VERSION"
	operandVersionEnvName  = "OPERAND_IMAGE_VERSION"
	operandImageEnvName    = "IMAGE"

	machineConfigNamespace = "openshift-config-managed"
	userConfigNamespace    = "openshift-config"

	maxRetries = 15
)

// static environment variables from operator deployment
var (
	csiSnapshotControllerImage = os.Getenv(targetNameController)

	operatorVersion = os.Getenv(operatorVersionEnvName)

	crdNames = []string{"volumesnapshotclasses.snapshot.storage.k8s.io", "volumesnapshotcontents.snapshot.storage.k8s.io", "volumesnapshots.snapshot.storage.k8s.io"}
)

type csiSnapshotOperator struct {
	vStore *versionStore

	client        OperatorClient
	kubeClient    kubernetes.Interface
	versionGetter status.VersionGetter
	eventRecorder events.Recorder

	syncHandler func(ic string) error

	crdLister       apiextlistersv1beta1.CustomResourceDefinitionLister
	crdListerSyncer cache.InformerSynced
	crdClient       apiextclient.Interface

	queue workqueue.RateLimitingInterface

	stopCh <-chan struct{}
}

func NewCSISnapshotControllerOperator(
	client OperatorClient,
	crdInformer apiextinformersv1beta1.CustomResourceDefinitionInformer,
	crdClient apiextclient.Interface,
	kubeClient kubernetes.Interface,
	versionGetter status.VersionGetter,
	eventRecorder events.Recorder,
) *csiSnapshotOperator {
	csiOperator := &csiSnapshotOperator{
		client:        client,
		crdClient:     crdClient,
		kubeClient:    kubeClient,
		versionGetter: versionGetter,
		eventRecorder: eventRecorder,
		vStore:        newVersionStore(),
		queue:         workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "csi-snapshot-controller"),
	}

	for _, i := range []cache.SharedIndexInformer{
		crdInformer.Informer(),
	} {
		i.AddEventHandler(csiOperator.eventHandler())
	}

	csiOperator.syncHandler = csiOperator.sync

	csiOperator.crdLister = crdInformer.Lister()
	csiOperator.crdListerSyncer = crdInformer.Informer().HasSynced

	csiOperator.vStore.Set("operator", os.Getenv("RELEASE_VERSION"))

	return csiOperator
}

func (c *csiSnapshotOperator) Run(workers int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	c.stopCh = stopCh

	for i := 0; i < workers; i++ {
		go wait.Until(c.worker, time.Second, stopCh)
	}
	<-stopCh
}

func (c *csiSnapshotOperator) sync(key string) error {
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

	err := c.syncCustomResourceDefinitions()
	if err != nil {
		return err
	}

	return nil
}

func (c *csiSnapshotOperator) enqueue(obj interface{}) {
	// we're filtering out config maps that are "leader" based and we don't have logic around them
	// resyncing on these causes the operator to sync every 14s for no good reason
	if cm, ok := obj.(*corev1.ConfigMap); ok && cm.GetAnnotations() != nil && cm.GetAnnotations()[resourcelock.LeaderElectionRecordAnnotationKey] != "" {
		return
	}
	workQueueKey := fmt.Sprintf("%s/%s", targetNamespace, targetName)
	c.queue.Add(workQueueKey)
}

func (c *csiSnapshotOperator) eventHandler() cache.ResourceEventHandler {
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			c.enqueue(obj)
		},
		UpdateFunc: func(old, new interface{}) {
			c.enqueue(new)
		},
		DeleteFunc: func(obj interface{}) {
			c.enqueue(obj)
		},
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

	err := c.syncHandler(key.(string))
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
