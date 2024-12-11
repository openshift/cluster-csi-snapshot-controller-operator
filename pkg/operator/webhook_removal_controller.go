package operator

import (
	"context"
	"strings"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/klog/v2"
)

var (
	csiSnapshotConditionPrefix       = "CSISnapshotWebhook"
	webHookControllerConditionPrefix = "WebhookController"
)

type webhookRemovalController struct {
	name                 string
	manifests            resourceapply.AssetFunc
	operatorClient       v1helpers.OperatorClient
	guestCompositeClient *resourceapply.ClientHolder
	mgmtCompositeClient  *resourceapply.ClientHolder
	eventRecorder        events.Recorder
}

func NewWebhookRemovalController(
	name string,
	manifests resourceapply.AssetFunc,
	operatorClient v1helpers.OperatorClient,
	guestCompositeClient *resourceapply.ClientHolder,
	mgmtCompositeClient *resourceapply.ClientHolder,
	eventRecorder events.Recorder,
) factory.Controller {
	c := &webhookRemovalController{
		name:                 name,
		manifests:            manifests,
		operatorClient:       operatorClient,
		guestCompositeClient: guestCompositeClient,
		mgmtCompositeClient:  mgmtCompositeClient,
		eventRecorder:        eventRecorder,
	}

	return factory.New().WithInformers().WithSync(c.sync).ResyncEvery(time.Minute).ToController(
		c.name,
		eventRecorder.WithComponentSuffix("webhook-removal-controller-"),
	)
}

func (c *webhookRemovalController) sync(ctx context.Context, syncContext factory.SyncContext) error {
	spec, status, _, err := c.operatorClient.GetOperatorState()
	if err != nil {
		return err
	}

	if spec.ManagementState != operatorv1.Managed {
		return nil
	}

	guestAssetsTobeRemoved := []string{
		"rbac/webhook_clusterrole.yaml",
		"rbac/webhook_clusterrolebinding.yaml",
		"webhook_config.yaml",
	}
	guestResourceRemovalResult := resourceapply.DeleteAll(ctx, c.guestCompositeClient, c.eventRecorder, c.manifests, guestAssetsTobeRemoved...)
	allErrors := []error{}

	for _, result := range guestResourceRemovalResult {
		if result.Error != nil {
			allErrors = append(allErrors, result.Error)
		}
	}

	mgmtAssetsTobeRemoved := []string{
		"webhook_serviceaccount.yaml",
		"webhook_service.yaml",
		"webhook_deployment_pdb.yaml",
		"webhook_deployment.yaml",
	}
	mgmtResouceRemovalResult := resourceapply.DeleteAll(ctx, c.mgmtCompositeClient, c.eventRecorder, c.manifests, mgmtAssetsTobeRemoved...)
	for _, result := range mgmtResouceRemovalResult {
		if result.Error != nil {
			allErrors = append(allErrors, result.Error)
		}
	}

	if len(allErrors) > 0 {
		return utilerrors.NewAggregate(allErrors)
	}
	return c.removeConditions(ctx, status)
}

// removing conditions related to webhook controller
func (c *webhookRemovalController) removeConditions(ctx context.Context, status *operatorv1.OperatorStatus) error {
	updateFuncs := []v1helpers.UpdateStatusFunc{}
	matchingConditions := []operatorv1.OperatorCondition{}

	originalConditions := status.DeepCopy().Conditions
	for _, condition := range originalConditions {
		if strings.HasPrefix(condition.Type, csiSnapshotConditionPrefix) || strings.HasPrefix(condition.Type, webHookControllerConditionPrefix) {
			klog.Infof("Removing condition %s", condition.Type)
			matchingConditions = append(matchingConditions, condition)
		}
	}
	updateFuncs = append(updateFuncs, func(status *operatorv1.OperatorStatus) error {
		for _, condition := range matchingConditions {
			v1helpers.RemoveOperatorCondition(&status.Conditions, condition.Type)
		}
		return nil
	})
	// also add a condition to indicate that the operator is disabled
	_, _, err := v1helpers.UpdateStatus(ctx, c.operatorClient, updateFuncs...)
	return err
}
