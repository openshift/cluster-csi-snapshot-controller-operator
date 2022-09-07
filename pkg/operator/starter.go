package operator

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	configclient "github.com/openshift/client-go/config/clientset/versioned"
	configinformer "github.com/openshift/client-go/config/informers/externalversions"
	"github.com/openshift/cluster-csi-snapshot-controller-operator/assets"
	"github.com/openshift/library-go/pkg/config/client"
	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/csi/csidrivercontrollerservicecontroller"
	dc "github.com/openshift/library-go/pkg/operator/deploymentcontroller"
	goc "github.com/openshift/library-go/pkg/operator/genericoperatorclient"
	"github.com/openshift/library-go/pkg/operator/loglevel"
	"github.com/openshift/library-go/pkg/operator/management"
	"github.com/openshift/library-go/pkg/operator/managementstatecontroller"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	staticcontrollercommon "github.com/openshift/library-go/pkg/operator/staticpod/controller/common"
	"github.com/openshift/library-go/pkg/operator/staticresourcecontroller"
	"github.com/openshift/library-go/pkg/operator/status"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	kubeclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"k8s.io/klog/v2"
)

const (
	targetName              = "csi-snapshot-controller"
	defaultOperandNamespace = "openshift-cluster-storage-operator"

	operatorVersionEnvName = "OPERATOR_IMAGE_VERSION"
	operandVersionEnvName  = "OPERAND_IMAGE_VERSION"
	operandImageEnvName    = "OPERAND_IMAGE"
	webhookImageEnvName    = "WEBHOOK_IMAGE"

	resync = 20 * time.Minute
)

func RunOperator(ctx context.Context, controllerConfig *controllercmd.ControllerContext, guestKubeConfigFile string) error {
	isHyperShift := guestKubeConfigFile != ""
	// Kubeconfig received through service account or --kubeconfig arg
	// is for the management cluster.
	controlPlaneKubeConfig := controllerConfig.KubeConfig
	controlPlaneKubeClient, err := kubeclient.NewForConfig(rest.AddUserAgent(controlPlaneKubeConfig, targetName))
	if err != nil {
		return err
	}
	controlPlaneNamespace := defaultOperandNamespace
	// Guest kubeconfig is the same as the management cluster one unless guestKubeConfigFile is provided
	guestKubeClient := controlPlaneKubeClient
	guestKubeConfig := controlPlaneKubeConfig
	if isHyperShift {
		guestKubeConfig, err = client.GetKubeConfigOrInClusterConfig(guestKubeConfigFile, nil)
		if err != nil {
			return fmt.Errorf("failed to use guest kubeconfig %s: %s", guestKubeConfigFile, err)
		}
		controlPlaneNamespace = controllerConfig.OperatorNamespace
		guestKubeClient = kubeclient.NewForConfigOrDie(rest.AddUserAgent(guestKubeConfig, targetName))
	}

	controlPlaneInformersForNamespaces := v1helpers.NewKubeInformersForNamespaces(controlPlaneKubeClient, "", controlPlaneNamespace)
	guestKubeInformersForNamespaces := v1helpers.NewKubeInformersForNamespaces(guestKubeClient, "", defaultOperandNamespace)

	// config.openshift.io client - use the guest cluster (Infrastructure)
	guestConfigClient, err := configclient.NewForConfig(rest.AddUserAgent(guestKubeConfig, targetName))
	if err != nil {
		return err
	}
	guestConfigInformers := configinformer.NewSharedInformerFactoryWithOptions(guestConfigClient, resync)

	// CRD client - use the guest cluster
	guestAPIExtClient, err := apiextclient.NewForConfig(rest.AddUserAgent(guestKubeConfig, targetName))
	if err != nil {
		return err
	}

	// Create GenericOperatorClient. This is used by the library-go controllers created down below
	// Operator CR is in the guest cluster.
	gvr := operatorv1.SchemeGroupVersion.WithResource("csisnapshotcontrollers")
	guestOperatorClient, dynamicInformers, err := goc.NewClusterScopedOperatorClientWithConfigName(guestKubeConfig, gvr, "cluster")
	if err != nil {
		return err
	}

	versionGetter := status.NewVersionGetter()

	namespacedAssetFunc := namespaceReplacer(assets.ReadFile, "${CONTROLPLANE_NAMESPACE}", controlPlaneNamespace)
	guestStaticResourceController := staticresourcecontroller.NewStaticResourceController(
		"CSISnapshotGuestStaticResourceController",
		namespacedAssetFunc,
		[]string{
			"volumesnapshots.yaml",
			"volumesnapshotcontents.yaml",
			"volumesnapshotclasses.yaml",
		},
		resourceapply.NewKubeClientHolder(guestKubeClient).WithAPIExtensionsClient(guestAPIExtClient),
		guestOperatorClient,
		controllerConfig.EventRecorder,
	)

	controlPlaneStaticResourcesController := staticresourcecontroller.NewStaticResourceController(
		"CSISnapshotStaticResourceController",
		namespacedAssetFunc,
		[]string{
			"serviceaccount.yaml",
			"webhook_service.yaml",
		},
		resourceapply.NewKubeClientHolder(controlPlaneKubeClient),
		guestOperatorClient,
		controllerConfig.EventRecorder,
	).WithConditionalResources(
		// Deploy PDBs everywhere except SNO
		namespacedAssetFunc,
		[]string{
			"csi_controller_deployment_pdb.yaml",
			"webhook_deployment_pdb.yaml",
		},
		func() bool {
			if isHyperShift {
				return true
			}
			isSNO, precheckSucceeded, err := staticcontrollercommon.NewIsSingleNodePlatformFn(guestConfigInformers.Config().V1().Infrastructures())()
			if err != nil {
				klog.Errorf("NewIsSingleNodePlatformFn failed: %v", err)
				return false
			}
			if !precheckSucceeded {
				klog.V(4).Infof("NewIsSingleNodePlatformFn precheck did not succeed, skipping")
				return false
			}
			return !isSNO
		},
		func() bool {
			if isHyperShift {
				return false
			}
			isSNO, precheckSucceeded, err := staticcontrollercommon.NewIsSingleNodePlatformFn(guestConfigInformers.Config().V1().Infrastructures())()
			if err != nil {
				klog.Errorf("NewIsSingleNodePlatformFn failed: %v", err)
				return false
			}
			if !precheckSucceeded {
				klog.V(4).Infof("NewIsSingleNodePlatformFn precheck did not succeed, skipping")
				return false
			}
			return isSNO
		},
	).AddKubeInformers(controlPlaneInformersForNamespaces)

	controllerDeploymentManifest, err := namespacedAssetFunc("csi_controller_deployment.yaml")
	if err != nil {
		return err
	}

	var webhookHooks []validatingWebhookConfigHook
	if isHyperShift {
		webhookHooks = []validatingWebhookConfigHook{
			hyperShiftSetWebhookService(),
		}
	}
	webhookController := NewValidatingWebhookConfigController(
		"WebhookController",
		guestOperatorClient,
		guestKubeClient,
		guestKubeInformersForNamespaces.InformersFor("").Admissionregistration().V1().ValidatingWebhookConfigurations(),
		namespacedAssetFunc,
		"webhook_config.yaml",
		controllerConfig.EventRecorder,
		webhookHooks,
	)

	var deploymentHooks []dc.DeploymentHookFunc
	if isHyperShift {
		deploymentHooks = []dc.DeploymentHookFunc{
			hyperShiftReplaceNamespaceHook(controlPlaneNamespace),
			hyperShiftRemoveNodeSelector(),
			hyperShiftAddKubeConfigVolume("admin-kubeconfig"), // TODO: use dedicated secret for Snapshots
		}
	} else {
		// Standalone OCP
		deploymentHooks = []dc.DeploymentHookFunc{
			csidrivercontrollerservicecontroller.WithControlPlaneTopologyHook(guestConfigInformers),
			csidrivercontrollerservicecontroller.WithReplicasHook(
				guestKubeInformersForNamespaces.InformersFor("").Core().V1().Nodes().Lister(),
			),
		}
	}
	controllerDeploymentController := dc.NewDeploymentController(
		"CSISnapshotController",
		controllerDeploymentManifest,
		controllerConfig.EventRecorder,
		guestOperatorClient,
		controlPlaneKubeClient,
		controlPlaneInformersForNamespaces.InformersFor(controlPlaneNamespace).Apps().V1().Deployments(),
		[]factory.Informer{
			guestKubeInformersForNamespaces.InformersFor("").Core().V1().Nodes().Informer(),
			guestConfigInformers.Config().V1().Infrastructures().Informer(),
		},
		[]dc.ManifestHookFunc{
			replacePlaceholdersHook(os.Getenv(operandImageEnvName)),
		},
		deploymentHooks...,
	)

	webhookDeploymentManifest, err := namespacedAssetFunc("webhook_deployment.yaml")
	if err != nil {
		return err
	}
	webhookDeploymentController := dc.NewDeploymentController(
		// Name of this controller must match SISnapshotWebhookController from 4.11
		// so it "adopts" its conditions during upgrade
		"CSISnapshotWebhookController",
		webhookDeploymentManifest,
		controllerConfig.EventRecorder,
		guestOperatorClient,
		controlPlaneKubeClient,
		controlPlaneInformersForNamespaces.InformersFor(controlPlaneNamespace).Apps().V1().Deployments(),
		[]factory.Informer{
			guestKubeInformersForNamespaces.InformersFor("").Core().V1().Nodes().Informer(),
			guestConfigInformers.Config().V1().Infrastructures().Informer(),
		},
		[]dc.ManifestHookFunc{
			replacePlaceholdersHook(os.Getenv(webhookImageEnvName)),
		},
		deploymentHooks...,
	)

	versionController := NewVersionController(
		"VersionController",
		guestOperatorClient,
		versionGetter,
		controllerConfig.EventRecorder,
		"CSISnapshotControllerAvailable",
		"CSISnapshotControllerProgressing",
		os.Getenv(operatorVersionEnvName),
		os.Getenv(operandVersionEnvName),
	)

	clusterOperatorStatus := status.NewClusterOperatorStatusController(
		targetName,
		[]configv1.ObjectReference{
			{Resource: "namespaces", Name: defaultOperandNamespace},
			{Group: operatorv1.GroupName, Resource: "csisnapshotcontrollers", Name: "cluster"},
		},
		guestConfigClient.ConfigV1(),
		guestConfigInformers.Config().V1().ClusterOperators(),
		guestOperatorClient,
		versionGetter,
		controllerConfig.EventRecorder,
	)

	// This is the only controller that sets Upgradeable condition
	cndController := NewConditionController(
		"ConditionController",
		guestOperatorClient,
		controllerConfig.EventRecorder,
		[]operatorv1.OperatorCondition{
			{
				// The condition name should match the same condition in previous OCP release (4.11).
				Type:   "CSISnapshotControllerUpgradeable",
				Status: operatorv1.ConditionTrue,
			},
		},
	)

	logLevelController := loglevel.NewClusterOperatorLoggingController(guestOperatorClient, controllerConfig.EventRecorder)
	managementStateController := managementstatecontroller.NewOperatorManagementStateController(targetName, guestOperatorClient, controllerConfig.EventRecorder)
	management.SetOperatorNotRemovable()

	klog.Info("Starting the Informers.")
	for _, informer := range []interface {
		Start(stopCh <-chan struct{})
	}{
		dynamicInformers,
		guestConfigInformers,
		controlPlaneInformersForNamespaces,
		guestKubeInformersForNamespaces,
	} {
		informer.Start(ctx.Done())
	}

	klog.Info("Starting the controllers")
	for _, controller := range []interface {
		Run(ctx context.Context, workers int)
	}{
		clusterOperatorStatus,
		logLevelController,
		managementStateController,
		controllerDeploymentController,
		webhookDeploymentController,
		versionController,
		cndController,
		guestStaticResourceController,
		controlPlaneStaticResourcesController,
		webhookController,
	} {
		if controller != nil {
			go controller.Run(ctx, 1)
		}
	}

	<-ctx.Done()

	return fmt.Errorf("stopped")
}

func replacePlaceholdersHook(imageName string) dc.ManifestHookFunc {
	return func(spec *operatorv1.OperatorSpec, manifest []byte) ([]byte, error) {
		pairs := []string{
			"${OPERAND_IMAGE}", imageName,
		}
		logLevel := loglevel.LogLevelToVerbosity(spec.LogLevel)
		pairs = append(pairs, "${LOG_LEVEL}", fmt.Sprint(logLevel))

		replaced := strings.NewReplacer(pairs...).Replace(string(manifest))
		return []byte(replaced), nil
	}
}

func hyperShiftReplaceNamespaceHook(operandNamespace string) dc.DeploymentHookFunc {
	return func(_ *operatorv1.OperatorSpec, deployment *appsv1.Deployment) error {
		deployment.Namespace = operandNamespace
		return nil
	}
}

func hyperShiftRemoveNodeSelector() dc.DeploymentHookFunc {
	return func(_ *operatorv1.OperatorSpec, deployment *appsv1.Deployment) error {
		deployment.Spec.Template.Spec.NodeSelector = map[string]string{}
		return nil
	}
}

func hyperShiftAddKubeConfigVolume(secretName string) dc.DeploymentHookFunc {
	return func(_ *operatorv1.OperatorSpec, deployment *appsv1.Deployment) error {
		// inject --kubeconfig arg
		controllerSpec := &deployment.Spec.Template.Spec
		controllerSpec.Containers[0].Args = append(controllerSpec.Containers[0].Args, "--kubeconfig=/etc/kubernetes/kubeconfig")

		// Inject Secrets volume with the kubeconfig + its mount
		kubeConfigVolume := v1.Volume{
			Name: "kubeconfig",
			VolumeSource: v1.VolumeSource{
				Secret: &v1.SecretVolumeSource{
					// TODO: use a snapshot-controller specific kubeconfig
					SecretName: secretName,
				},
			},
		}
		if controllerSpec.Volumes == nil {
			controllerSpec.Volumes = []v1.Volume{}
		}
		controllerSpec.Volumes = append(controllerSpec.Volumes, kubeConfigVolume)

		kubeConfigMount := v1.VolumeMount{
			Name:      "kubeconfig",
			ReadOnly:  true,
			MountPath: "/etc/kubernetes",
		}
		if controllerSpec.Containers[0].VolumeMounts == nil {
			controllerSpec.Containers[0].VolumeMounts = []v1.VolumeMount{}
		}
		controllerSpec.Containers[0].VolumeMounts = append(controllerSpec.Containers[0].VolumeMounts, kubeConfigMount)

		return nil
	}
}

func hyperShiftSetWebhookService() validatingWebhookConfigHook {
	return func(configuration *admissionregistrationv1.ValidatingWebhookConfiguration) error {
		for i := range configuration.Webhooks {
			webhook := &configuration.Webhooks[i]
			webhook.ClientConfig.Service = nil
			// Name of csi-snapshot-webhook service in the same namespace as the guest API server.
			url := "https://csi-snapshot-webhook"
			webhook.ClientConfig.URL = &url
		}

		return nil
	}
}

func namespaceReplacer(assetFunc resourceapply.AssetFunc, placeholder, namespace string) resourceapply.AssetFunc {
	return func(name string) ([]byte, error) {
		asset, err := assetFunc(name)
		if err != nil {
			return asset, err
		}
		asset = bytes.ReplaceAll(asset, []byte(placeholder), []byte(namespace))
		return asset, nil
	}
}
