package operator

import (
	"context"
	"strings"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/status"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"k8s.io/klog/v2"
)

// versionController updates versionGetter version when snapshot DeploymentController gets Available=true +
// Progressing=false, i.e. at the end of snapshot controller upgrade.
type versionController struct {
	name          string
	client        v1helpers.OperatorClient
	versionGetter status.VersionGetter
	eventRecorder events.Recorder

	availableConditionName     string
	progressingConditionName   string
	operatorVersion            string
	operandVersion             string
	csiSnapshotControllerImage string
}

func NewVersionController(
	name string,
	client v1helpers.OperatorClient,
	versionGetter status.VersionGetter,
	eventRecorder events.Recorder,
	availableConditionName string,
	progressingConditionName string,
	operatorVersion string,
	operandVersion string,
) factory.Controller {
	c := &versionController{
		name:                     name,
		client:                   client,
		versionGetter:            versionGetter,
		eventRecorder:            eventRecorder,
		availableConditionName:   availableConditionName,
		progressingConditionName: progressingConditionName,
		operatorVersion:          operatorVersion,
		operandVersion:           operandVersion,
	}

	return factory.New().WithInformers(
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

func (c *versionController) sync(ctx context.Context, syncContext factory.SyncContext) error {
	spec, status, _, err := c.client.GetOperatorState()
	if err != nil {
		return err
	}

	if spec.ManagementState != operatorv1.Managed {
		return nil
	}

	// Only set versions if we reached the desired state
	isAvailable := v1helpers.IsOperatorConditionTrue(status.Conditions, c.availableConditionName)
	isProgressing := v1helpers.IsOperatorConditionTrue(status.Conditions, c.progressingConditionName)
	if isAvailable && !isProgressing {
		c.setVersion("operator", c.operatorVersion)
		c.setVersion("csi-snapshot-controller", c.operandVersion)
	}

	return nil
}

func (c *versionController) setVersion(operandName, version string) {
	if c.versionChanged(operandName, version) {
		klog.V(2).Infof("Setting version %s / %s", operandName, version)
		c.versionGetter.SetVersion(operandName, version)
	}
}

func (c *versionController) versionChanged(operandName, version string) bool {
	return c.versionGetter.GetVersions()[operandName] != version
}
