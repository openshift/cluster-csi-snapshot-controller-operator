package webhookdeployment

import (
	"context"

	operatorapi "github.com/openshift/api/operator/v1"
	"github.com/openshift/cluster-csi-snapshot-controller-operator/assets"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/resource/resourcemerge"
	"github.com/openshift/library-go/pkg/operator/resource/resourceread"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	admissionnformersv1 "k8s.io/client-go/informers/admissionregistration/v1"
	"k8s.io/client-go/kubernetes"
)

type csiSnapshotWebhookController struct {
	client           v1helpers.OperatorClient
	kubeClient       kubernetes.Interface
	eventRecorder    events.Recorder
	performanceCache resourceapply.ResourceCache
}

const (
	// Name of this controller must *not* match CSISnapshotWebhookController from 4.11,
	// because those are owned by DdeploymentController that creates the webhook Deployment.
	WebhookControllerName = "CSISnapshotWebhookApplyController"
	webhookAsset          = "webhook_config.yaml"
)

var (
	admissionScheme = runtime.NewScheme()
	admissionCodecs = serializer.NewCodecFactory(admissionScheme)
)

func init() {
	// Register admission/v1 schema for ValidatingWebhookConfiguration decoding
	if err := admissionv1.AddToScheme(admissionScheme); err != nil {
		panic(err)
	}
}

// NewCSISnapshotWebhookController returns a controller that creates and manages Deployment with CSI snapshot webhook.
// In theory this could be a StaticController, but we expect the webhook will be dynamic in HyperShift.
func NewCSISnapshotWebhookController(
	client v1helpers.OperatorClient,
	webhookInformer admissionnformersv1.ValidatingWebhookConfigurationInformer,
	kubeClient kubernetes.Interface,
	eventRecorder events.Recorder,
) factory.Controller {
	c := &csiSnapshotWebhookController{
		client:           client,
		kubeClient:       kubeClient,
		eventRecorder:    eventRecorder,
		performanceCache: resourceapply.NewResourceCache(),
	}

	return factory.New().WithSync(c.sync).WithSyncDegradedOnError(client).WithInformers(
		client.Informer(),
		webhookInformer.Informer(),
	).ToController(WebhookControllerName, eventRecorder.WithComponentSuffix(WebhookControllerName))
}

func (c *csiSnapshotWebhookController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	opSpec, _, _, err := c.client.GetOperatorState()
	if err != nil {
		return err
	}
	if opSpec.ManagementState != operatorapi.Managed {
		return nil
	}

	webhookConfig, err := getWebhookConfig()
	if err != nil {
		return err
	}
	webhookConfig, _, err = resourceapply.ApplyValidatingWebhookConfigurationImproved(ctx, c.kubeClient.AdmissionregistrationV1(), syncCtx.Recorder(), webhookConfig, c.performanceCache)
	if err != nil {
		return err
	}

	_, _, err = v1helpers.UpdateStatus(ctx, c.client,
		func(newStatus *operatorapi.OperatorStatus) error {
			resourcemerge.SetValidatingWebhooksConfigurationGeneration(&newStatus.Generations, webhookConfig)
			return nil
		},
	)
	return err
}

func getWebhookConfig() (*admissionv1.ValidatingWebhookConfiguration, error) {
	webhookBytes, err := assets.ReadFile(webhookAsset)
	if err != nil {
		return nil, err
	}
	webhook := resourceread.ReadValidatingWebhookConfigurationV1OrDie(webhookBytes)
	resourceapply.SetSpecHashAnnotation(&webhook.ObjectMeta, webhook.Webhooks)
	return webhook, nil
}
