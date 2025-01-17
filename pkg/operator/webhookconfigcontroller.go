package operator

import (
	"context"
	"strings"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/resource/resourceread"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	admissionregistrationinformer "k8s.io/client-go/informers/admissionregistration/v1"
	"k8s.io/client-go/kubernetes"
	admissionregistrationclientv1 "k8s.io/client-go/kubernetes/typed/admissionregistration/v1"
)

// validatingWebhookConfigController manages a ValidatinWebhookConfiguration object.
// StaticResourceController is not enough here, because we need to replace namespaces in HyperShift, i.e. hooks.
type validatingWebhookConfigHook func(configuration *admissionregistrationv1.ValidatingWebhookConfiguration) error

type validatingWebhookConfigController struct {
	name                string
	client              v1helpers.OperatorClient
	webhookConfigClient admissionregistrationclientv1.AdmissionregistrationV1Interface
	assetFunc           resourceapply.AssetFunc
	asset               string
	eventRecorder       events.Recorder

	cache resourceapply.ResourceCache
	hooks []validatingWebhookConfigHook
}

func NewValidatingWebhookConfigController(
	name string,
	client v1helpers.OperatorClient,
	kubeCient kubernetes.Interface,
	webhookInformer admissionregistrationinformer.ValidatingWebhookConfigurationInformer,
	assetFunc resourceapply.AssetFunc,
	asset string,
	eventRecorder events.Recorder,
	hooks []validatingWebhookConfigHook,
) factory.Controller {
	c := &validatingWebhookConfigController{
		name:                name,
		client:              client,
		webhookConfigClient: kubeCient.AdmissionregistrationV1(),
		assetFunc:           assetFunc,
		asset:               asset,
		eventRecorder:       eventRecorder,
		cache:               resourceapply.NewResourceCache(),
		hooks:               hooks,
	}

	return factory.New().WithInformers(
		client.Informer(),
		webhookInformer.Informer(),
	).WithSync(
		c.sync,
	).ResyncEvery(
		time.Minute,
	).WithSyncDegradedOnError(
		client,
	).ToController(
		c.name,
		eventRecorder.WithComponentSuffix(strings.ToLower(name)+"-webhook-config-controller-"),
	)
}

func (c *validatingWebhookConfigController) sync(ctx context.Context, syncContext factory.SyncContext) error {
	spec, _, _, err := c.client.GetOperatorState()
	if err != nil {
		return err
	}

	if spec.ManagementState != operatorv1.Managed {
		return nil
	}

	asset, err := c.assetFunc(c.asset)
	if err != nil {
		return err
	}
	vwc := resourceread.ReadValidatingWebhookConfigurationV1OrDie(asset)

	for _, hook := range c.hooks {
		if err := hook(vwc); err != nil {
			return err
		}
	}
	_, _, err = resourceapply.ApplyValidatingWebhookConfigurationImproved(ctx, c.webhookConfigClient, c.eventRecorder, vwc, c.cache)
	return err
}
