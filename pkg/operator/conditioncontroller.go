package operator

import (
	"context"
	"strings"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
)

// conditionController sets given conditions to given state
type conditionController struct {
	name          string
	client        v1helpers.OperatorClient
	eventRecorder events.Recorder

	conditions []operatorv1.OperatorCondition
}

func NewConditionController(
	name string,
	client v1helpers.OperatorClient,
	eventRecorder events.Recorder,
	conditions []operatorv1.OperatorCondition,
) factory.Controller {
	c := &conditionController{
		name:          name,
		client:        client,
		eventRecorder: eventRecorder,
		conditions:    conditions,
	}

	return factory.New().WithInformers(
		client.Informer(),
	).WithSync(
		c.sync,
	).ResyncEvery(
		time.Minute,
	).ToController(
		c.name,
		eventRecorder.WithComponentSuffix(strings.ToLower(name)+"-condition-controller-"),
	)
}

func (c *conditionController) sync(ctx context.Context, syncContext factory.SyncContext) error {
	spec, _, _, err := c.client.GetOperatorState()
	if err != nil {
		return err
	}

	if spec.ManagementState != operatorv1.Managed {
		return nil
	}

	var updateFuncs []v1helpers.UpdateStatusFunc
	for _, cnd := range c.conditions {
		updateFuncs = append(updateFuncs, v1helpers.UpdateConditionFn(cnd))
	}
	_, _, err = v1helpers.UpdateStatus(ctx, c.client, updateFuncs...)
	return err
}
