package webhookdeployment

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	operatorapi "github.com/openshift/api/operator/v1"
	"github.com/openshift/cluster-csi-snapshot-controller-operator/pkg/generated"
	"github.com/openshift/cluster-csi-snapshot-controller-operator/pkg/operatorclient"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/loglevel"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/resource/resourcemerge"
	"github.com/openshift/library-go/pkg/operator/resource/resourceread"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	admissionnformersv1 "k8s.io/client-go/informers/admissionregistration/v1"
	appsinformersv1 "k8s.io/client-go/informers/apps/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/workqueue"
)

type csiSnapshotWebhookController struct {
	client        operatorclient.OperatorClient
	kubeClient    kubernetes.Interface
	eventRecorder events.Recorder

	queue workqueue.RateLimitingInterface

	csiSnapshotWebhookImage string
}

const (
	WebhookControllerName = "CSISnapshotWebhookController"
	webhookVersionName    = "CSISnapshotWebhookDeployment"
	deploymentAsset       = "webhook_deployment.yaml"
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
func NewCSISnapshotWebhookController(
	client operatorclient.OperatorClient,
	deployInformer appsinformersv1.DeploymentInformer,
	webhookInformer admissionnformersv1.ValidatingWebhookConfigurationInformer,
	kubeClient kubernetes.Interface,
	eventRecorder events.Recorder,
	csiSnapshotWebhookImage string,
) factory.Controller {
	c := &csiSnapshotWebhookController{
		client:                  client,
		kubeClient:              kubeClient,
		eventRecorder:           eventRecorder,
		queue:                   workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "csi-snapshot-controller"),
		csiSnapshotWebhookImage: csiSnapshotWebhookImage,
	}

	return factory.New().WithSync(c.sync).WithSyncDegradedOnError(client).WithInformers(
		client.Informer(),
		deployInformer.Informer(),
		webhookInformer.Informer(),
	).ToController(WebhookControllerName, eventRecorder.WithComponentSuffix(WebhookControllerName))
}

func (c *csiSnapshotWebhookController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	opSpec, opStatus, _, err := c.client.GetOperatorState()
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if opSpec.ManagementState != operatorapi.Managed {
		return nil
	}

	deployment, err := c.getDeployment(opSpec)
	if err != nil {
		// This will set Degraded condition
		return err
	}
	lastGeneration := resourcemerge.ExpectedDeploymentGeneration(deployment, opStatus.Generations)
	deployment, _, err = resourceapply.ApplyDeployment(c.kubeClient.AppsV1(), syncCtx.Recorder(), deployment, lastGeneration)
	if err != nil {
		// This will set Degraded condition
		return err
	}

	webhookConfig, err := getWebhookConfig()
	if err != nil {
		return err
	}
	lastWebhookGeneration := resourcemerge.ExpectedValidatingWebhooksConfiguration(webhookConfig.Name, opStatus.Generations)
	webhookConfig, _, err = resourceapply.ApplyValidatingWebhookConfiguration(c.kubeClient.AdmissionregistrationV1(), syncCtx.Recorder(), webhookConfig, lastWebhookGeneration)
	if err != nil {
		return err
	}

	// Compute status
	// Available: at least one replica is running
	deploymentAvailable := operatorapi.OperatorCondition{
		Type: WebhookControllerName + operatorapi.OperatorStatusTypeAvailable,
	}
	if deployment.Status.AvailableReplicas > 0 {
		deploymentAvailable.Status = operatorapi.ConditionTrue
	} else {
		deploymentAvailable.Status = operatorapi.ConditionFalse
		deploymentAvailable.Reason = "Deploying"
		deploymentAvailable.Message = "Waiting for a validating webhook Deployment pod to start"
	}

	// Not progressing: all replicas are at the latest version && Deployment generation matches
	deploymentProgressing := operatorapi.OperatorCondition{
		Type: WebhookControllerName + operatorapi.OperatorStatusTypeProgressing,
	}
	if deployment.Status.ObservedGeneration != deployment.Generation {
		deploymentProgressing.Status = operatorapi.ConditionTrue
		deploymentProgressing.Reason = "Deploying"
		msg := fmt.Sprintf("desired generation %d, current generation %d", deployment.Generation, deployment.Status.ObservedGeneration)
		deploymentProgressing.Message = msg
	} else {
		if deployment.Spec.Replicas != nil {
			if deployment.Status.UpdatedReplicas == *deployment.Spec.Replicas {
				deploymentProgressing.Status = operatorapi.ConditionFalse
			} else {
				msg := fmt.Sprintf("%d out of %d pods running", deployment.Status.UpdatedReplicas, *deployment.Spec.Replicas)
				deploymentProgressing.Status = operatorapi.ConditionTrue
				deploymentProgressing.Reason = "Deploying"
				deploymentProgressing.Message = msg
			}
		}
	}

	updateGenerationFn := func(newStatus *operatorapi.OperatorStatus) error {
		resourcemerge.SetDeploymentGeneration(&newStatus.Generations, deployment)
		resourcemerge.SetValidatingWebhooksConfigurationGeneration(&newStatus.Generations, webhookConfig)
		return nil
	}

	_, _, err = v1helpers.UpdateStatus(c.client,
		v1helpers.UpdateConditionFn(deploymentAvailable),
		v1helpers.UpdateConditionFn(deploymentProgressing),
		updateGenerationFn,
	)
	return err
}

func (c *csiSnapshotWebhookController) getDeployment(opSpec *operatorapi.OperatorSpec) (*appsv1.Deployment, error) {
	deploymentString := string(generated.MustAsset(deploymentAsset))

	// Replace image
	deploymentString = strings.ReplaceAll(deploymentString, "${WEBHOOK_IMAGE}", c.csiSnapshotWebhookImage)
	// Replace log level
	if !loglevel.ValidLogLevel(opSpec.LogLevel) {
		return nil, fmt.Errorf("logLevel %q is not a valid log level", opSpec.LogLevel)
	}
	logLevel := loglevel.LogLevelToVerbosity(opSpec.LogLevel)
	deploymentString = strings.ReplaceAll(deploymentString, "${LOG_LEVEL}", strconv.Itoa(logLevel))

	deployment := resourceread.ReadDeploymentV1OrDie([]byte(deploymentString))
	return deployment, nil

}

func getWebhookConfig() (*admissionv1.ValidatingWebhookConfiguration, error) {
	webhookBytes := generated.MustAsset(webhookAsset)
	requiredObj, err := runtime.Decode(admissionCodecs.UniversalDecoder(admissionv1.SchemeGroupVersion), webhookBytes)
	if err != nil {
		return nil, err
	}

	webhook := requiredObj.(*admissionv1.ValidatingWebhookConfiguration)
	// Set hash of Webhooks[] to apply new ValidatingWebhookConfiguration when the asset changes on the operator update.
	resourceapply.SetSpecHashAnnotation(&webhook.ObjectMeta, webhook.Webhooks)
	return webhook, nil
}
