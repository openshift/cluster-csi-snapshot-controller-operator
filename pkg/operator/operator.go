package operator

import (
	"context"
	"fmt"
	"strings"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	configlisterv1 "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/cluster-csi-snapshot-controller-operator/pkg/operatorclient"
	"github.com/openshift/library-go/pkg/controller/factory"
	appsinformersv1 "k8s.io/client-go/informers/apps/v1"
	coreinformersv1 "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	corelistersv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"

	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/status"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
)

const (
	snapshotControllerName = "CSISnapshotController"
	targetName             = "csi-snapshot-controller"
	targetNamespace        = "openshift-cluster-storage-operator"
	operatorNamespace      = "openshift-cluster-storage-operator"

	operatorVersionEnvName = "OPERATOR_IMAGE_VERSION"
	operandVersionEnvName  = "OPERAND_IMAGE_VERSION"
	operandImageEnvName    = "OPERAND_IMAGE"
	webhookImageEnvName    = "WEBHOOK_IMAGE"

	maxRetries = 15
)

type csiSnapshotController struct {
	name          string
	client        operatorclient.OperatorClient
	kubeClient    kubernetes.Interface
	versionGetter status.VersionGetter
	eventRecorder events.Recorder

	infraLister configlisterv1.InfrastructureLister
	nodeLister  corelistersv1.NodeLister

	operatorVersion            string
	operandVersion             string
	csiSnapshotControllerImage string
}

func NewCSISnapshotController(
	name string,
	client operatorclient.OperatorClient,
	nodeInformer coreinformersv1.NodeInformer,
	deployInformer appsinformersv1.DeploymentInformer,
	infraLister configlisterv1.InfrastructureLister,
	kubeClient kubernetes.Interface,
	versionGetter status.VersionGetter,
	eventRecorder events.Recorder,
	operatorVersion string,
	operandVersion string,
	csiSnapshotControllerImage string,
) factory.Controller {
	c := &csiSnapshotController{
		name:                       name,
		client:                     client,
		nodeLister:                 nodeInformer.Lister(),
		kubeClient:                 kubeClient,
		versionGetter:              versionGetter,
		eventRecorder:              eventRecorder,
		operatorVersion:            operatorVersion,
		operandVersion:             operandVersion,
		csiSnapshotControllerImage: csiSnapshotControllerImage,
		infraLister:                infraLister,
	}

	return factory.New().WithInformers(
		nodeInformer.Informer(),
		deployInformer.Informer(),
		client.Informer(),
	).WithSync(
		c.sync,
	).ResyncEvery(
		time.Minute,
	).ToController(
		c.name,
		eventRecorder.WithComponentSuffix(strings.ToLower(name)+"-deployment-controller-"),
	)
}

func (c *csiSnapshotController) sync(ctx context.Context, syncContext factory.SyncContext) error {
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

	if _, _, err := v1helpers.UpdateStatus(ctx, c.client, func(status *operatorv1.OperatorStatus) error {
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

func (c *csiSnapshotController) updateSyncError(status *operatorv1.OperatorStatus, err error) {
	if err != nil {
		degradedReason := "OperatorSync"
		v1helpers.SetOperatorCondition(&status.Conditions,
			operatorv1.OperatorCondition{
				Type:    conditionName(operatorv1.OperatorStatusTypeDegraded),
				Status:  operatorv1.ConditionTrue,
				Reason:  degradedReason,
				Message: err.Error(),
			})
	} else {
		v1helpers.SetOperatorCondition(&status.Conditions,
			operatorv1.OperatorCondition{
				Type:   conditionName(operatorv1.OperatorStatusTypeDegraded),
				Status: operatorv1.ConditionFalse,
			})
	}
}

func (c *csiSnapshotController) handleSync(instance *operatorv1.CSISnapshotController) error {
	deployment, err := c.syncDeployment(instance)
	if err != nil {
		return fmt.Errorf("failed to sync Deployments: %s", err)
	}
	if err := c.syncStatus(instance, deployment); err != nil {
		return fmt.Errorf("failed to sync status: %s", err)
	}
	return nil
}

func (c *csiSnapshotController) setVersion(operandName, version string) {
	if c.versionChanged(operandName, version) {
		c.versionGetter.SetVersion(operandName, version)
	}
}

func (c *csiSnapshotController) versionChanged(operandName, version string) bool {
	return c.versionGetter.GetVersions()[operandName] != version
}
