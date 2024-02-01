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
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
	"github.com/openshift/library-go/pkg/operator/csi/csidrivercontrollerservicecontroller"
	dc "github.com/openshift/library-go/pkg/operator/deploymentcontroller"
	"github.com/openshift/library-go/pkg/operator/events"
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
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	informercorev1 "k8s.io/client-go/informers/core/v1"
	kubeclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

const (
	targetName     = "csi-snapshot-controller"
	guestNamespace = "openshift-cluster-storage-operator"

	operatorVersionEnvName = "OPERATOR_IMAGE_VERSION"
	operandVersionEnvName  = "OPERAND_IMAGE_VERSION"
	operandImageEnvName    = "OPERAND_IMAGE"
	webhookImageEnvName    = "WEBHOOK_IMAGE"

	defaultPriorityClass     = "system-cluster-critical"
	hypershiftPriorityClass  = "hypershift-control-plane"
	hyperShiftPullSecretName = "pull-secret"
	webhookSecretName        = "csi-snapshot-webhook-secret"

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

	controlPlaneDynamicClient, err := dynamic.NewForConfig(rest.AddUserAgent(controlPlaneKubeConfig, targetName))
	if err != nil {
		return err
	}

	eventRecorder := controllerConfig.EventRecorder
	controlPlaneNamespace := controllerConfig.OperatorNamespace
	// Guest kubeconfig is the same as the management cluster one unless guestKubeConfigFile is provided
	guestKubeClient := controlPlaneKubeClient
	guestKubeConfig := controlPlaneKubeConfig
	if isHyperShift {
		guestKubeConfig, err = client.GetKubeConfigOrInClusterConfig(guestKubeConfigFile, nil)
		if err != nil {
			return fmt.Errorf("failed to use guest kubeconfig %s: %s", guestKubeConfigFile, err)
		}
		guestKubeClient = kubeclient.NewForConfigOrDie(rest.AddUserAgent(guestKubeConfig, targetName))

		// Create all events in the guest cluster.
		// Use name of the operator Deployment in the mgmt cluster + namespace in the guest cluster as the closest
		// approximation of the real involvedObject.
		controllerRef, err := events.GetControllerReferenceForCurrentPod(ctx, controlPlaneKubeClient, controlPlaneNamespace, nil)
		controllerRef.Namespace = guestNamespace
		if err != nil {
			klog.Warningf("unable to get owner reference (falling back to namespace): %v", err)
		}
		eventRecorder = events.NewKubeRecorder(guestKubeClient.CoreV1().Events(guestNamespace), targetName, controllerRef)
	}

	controlPlaneInformersForNamespaces := v1helpers.NewKubeInformersForNamespaces(controlPlaneKubeClient, "", controlPlaneNamespace)
	controlPlaneDynamicInformers := dynamicinformer.NewFilteredDynamicSharedInformerFactory(controlPlaneDynamicClient, resync, controlPlaneNamespace, nil)
	guestKubeInformersForNamespaces := v1helpers.NewKubeInformersForNamespaces(guestKubeClient, "", guestNamespace)

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

	// Get the known feature gates.
	desiredVersion := status.VersionForOperatorFromEnv()
	missingVersion := "0.0.1-snapshot"
	featureGateAccessor := featuregates.NewFeatureGateAccess(
		desiredVersion,
		missingVersion,
		guestConfigInformers.Config().V1().ClusterVersions(),
		guestConfigInformers.Config().V1().FeatureGates(),
		eventRecorder,
	)
	go featureGateAccessor.Run(ctx)
	go guestConfigInformers.Start(ctx.Done())

	select {
	case <-featureGateAccessor.InitialFeatureGatesObserved():
		featureGates, _ := featureGateAccessor.CurrentFeatureGates()
		klog.Infof("FeatureGates initialized: knownFeatureGates=%v", featureGates.KnownFeatures())
	case <-time.After(1 * time.Minute):
		klog.Errorf("timed out waiting for FeatureGate detection")
		return fmt.Errorf("timed out waiting for FeatureGate detection")
	}

	featureGates, err := featureGateAccessor.CurrentFeatureGates()
	if err != nil {
		return err
	}

	// Verify whether the VolumeGroupSnapshot feature is enabled or not. This variable will be
	// used to decided what resources will be deployed in the cluster.
	volumeGroupSnapshotAPIEnabled := featureGates.Enabled(configv1.FeatureGateVolumeGroupSnapshot)

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
		eventRecorder,
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
		eventRecorder,
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
			return false
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
		eventRecorder,
		webhookHooks,
	)

	priorityClass := defaultPriorityClass
	var deploymentHooks []dc.DeploymentHookFunc
	deploymentInformers := []factory.Informer{
		guestKubeInformersForNamespaces.InformersFor("").Core().V1().Nodes().Informer(),
		guestConfigInformers.Config().V1().Infrastructures().Informer(),
	}

	if isHyperShift {
		priorityClass = hypershiftPriorityClass

		// Create HostedControlPlane informer
		hcpGVR := schema.GroupVersionResource{
			Group:    "hypershift.openshift.io",
			Version:  "v1beta1",
			Resource: "hostedcontrolplanes",
		}
		hcpInformer := controlPlaneDynamicInformers.ForResource(hcpGVR)
		deploymentInformers = append(deploymentInformers, hcpInformer.Informer())

		deploymentHooks = []dc.DeploymentHookFunc{
			hyperShiftReplaceNamespaceHook(controlPlaneNamespace),
			hyperShiftAddKubeConfigVolume("service-network-admin-kubeconfig"), // TODO: use dedicated secret for Snapshots
			hyperShiftAddPullSecret(),
			hyperShiftControlPlaneIsolationHook(controlPlaneNamespace),
			hyperShiftColocationHook(controlPlaneNamespace),
			hyperShiftNodeSelectorHook(hcpInformer.Lister(), controlPlaneNamespace),
		}

	} else {
		// Standalone OCP
		deploymentHooks = []dc.DeploymentHookFunc{
			csidrivercontrollerservicecontroller.WithControlPlaneTopologyHook(guestConfigInformers),
			csidrivercontrollerservicecontroller.WithReplicasHook(
				guestKubeInformersForNamespaces.InformersFor("").Core().V1().Nodes().Lister(),
			),
			withVolumeGroupSnapshotWebhook(volumeGroupSnapshotAPIEnabled),
		}
	}
	controllerDeploymentController := dc.NewDeploymentController(
		"CSISnapshotController",
		controllerDeploymentManifest,
		eventRecorder,
		guestOperatorClient,
		controlPlaneKubeClient,
		controlPlaneInformersForNamespaces.InformersFor(controlPlaneNamespace).Apps().V1().Deployments(),
		deploymentInformers,
		[]dc.ManifestHookFunc{
			replacePlaceholdersHook(os.Getenv(operandImageEnvName), priorityClass),
		},
		deploymentHooks...,
	)

	webhookDeploymentManifest, err := namespacedAssetFunc("webhook_deployment.yaml")
	if err != nil {
		return err
	}
	var secretInformer informercorev1.SecretInformer
	if isHyperShift {
		secretInformer = controlPlaneInformersForNamespaces.InformersFor(controlPlaneNamespace).Core().V1().Secrets()
	} else {
		secretInformer = guestKubeInformersForNamespaces.InformersFor(controlPlaneNamespace).Core().V1().Secrets()
	}
	deploymentHooks = append(deploymentHooks, csidrivercontrollerservicecontroller.WithSecretHashAnnotationHook(
		guestNamespace,
		webhookSecretName,
		secretInformer,
	))
	webhookDeploymentController := dc.NewDeploymentController(
		// Name of this controller must match SISnapshotWebhookController from 4.11
		// so it "adopts" its conditions during upgrade
		"CSISnapshotWebhookController",
		webhookDeploymentManifest,
		eventRecorder,
		guestOperatorClient,
		controlPlaneKubeClient,
		controlPlaneInformersForNamespaces.InformersFor(controlPlaneNamespace).Apps().V1().Deployments(),
		[]factory.Informer{
			guestKubeInformersForNamespaces.InformersFor("").Core().V1().Nodes().Informer(),
			guestConfigInformers.Config().V1().Infrastructures().Informer(),
			secretInformer.Informer(),
		},
		[]dc.ManifestHookFunc{
			replacePlaceholdersHook(os.Getenv(webhookImageEnvName), priorityClass),
		},
		deploymentHooks...,
	)

	versionController := NewVersionController(
		"VersionController",
		guestOperatorClient,
		versionGetter,
		eventRecorder,
		"CSISnapshotControllerAvailable",
		"CSISnapshotControllerProgressing",
		os.Getenv(operatorVersionEnvName),
		os.Getenv(operandVersionEnvName),
	)

	clusterOperatorStatus := status.NewClusterOperatorStatusController(
		targetName,
		[]configv1.ObjectReference{
			{Resource: "namespaces", Name: guestNamespace},
			{Group: operatorv1.GroupName, Resource: "csisnapshotcontrollers", Name: "cluster"},
		},
		guestConfigClient.ConfigV1(),
		guestConfigInformers.Config().V1().ClusterOperators(),
		guestOperatorClient,
		versionGetter,
		eventRecorder,
	)

	// This is the only controller that sets Upgradeable condition
	cndController := NewConditionController(
		"ConditionController",
		guestOperatorClient,
		eventRecorder,
		[]operatorv1.OperatorCondition{
			{
				// The condition name should match the same condition in previous OCP release (4.11).
				Type:   "CSISnapshotControllerUpgradeable",
				Status: operatorv1.ConditionTrue,
			},
		},
	)

	logLevelController := loglevel.NewClusterOperatorLoggingController(guestOperatorClient, eventRecorder)
	managementStateController := managementstatecontroller.NewOperatorManagementStateController(targetName, guestOperatorClient, eventRecorder)
	management.SetOperatorNotRemovable()

	klog.Info("Starting the Informers.")
	for _, informer := range []interface {
		Start(stopCh <-chan struct{})
	}{
		controlPlaneDynamicInformers,
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

	return nil
}

func replacePlaceholdersHook(imageName, priorityClass string) dc.ManifestHookFunc {
	return func(spec *operatorv1.OperatorSpec, manifest []byte) ([]byte, error) {
		pairs := []string{
			"${OPERAND_IMAGE}", imageName,
			"${PRIORITY_CLASS}", priorityClass,
		}
		logLevel := loglevel.LogLevelToVerbosity(spec.LogLevel)
		pairs = append(pairs, "${LOG_LEVEL}", fmt.Sprint(logLevel))

		replaced := strings.NewReplacer(pairs...).Replace(string(manifest))
		return []byte(replaced), nil
	}
}

func hyperShiftNodeSelectorHook(hcpLister cache.GenericLister, controlPlaneNamespace string) dc.DeploymentHookFunc {
	return func(_ *operatorv1.OperatorSpec, d *appsv1.Deployment) error {
		nodeSelector, err := getHostedControlPlaneNodeSelector(hcpLister, controlPlaneNamespace)
		if err != nil {
			return err
		}
		d.Spec.Template.Spec.NodeSelector = nodeSelector
		return nil
	}
}

func getHostedControlPlaneNodeSelector(hostedControlPlaneLister cache.GenericLister, namespace string) (map[string]string, error) {
	hcp, err := getHostedControlPlane(hostedControlPlaneLister, namespace)
	if err != nil {
		return nil, err
	}
	nodeSelector, exists, err := unstructured.NestedStringMap(hcp.UnstructuredContent(), "spec", "nodeSelector")
	if !exists {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	klog.V(4).Infof("Using node selector %v", nodeSelector)
	return nodeSelector, nil
}

func getHostedControlPlane(hostedControlPlaneLister cache.GenericLister, namespace string) (*unstructured.Unstructured, error) {
	list, err := hostedControlPlaneLister.ByNamespace(namespace).List(labels.Everything())
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("no HostedControlPlane found in namespace %s", namespace)
	}
	if len(list) > 1 {
		return nil, fmt.Errorf("more than one HostedControlPlane found in namespace %s", namespace)
	}

	hcp := list[0].(*unstructured.Unstructured)
	if hcp == nil {
		return nil, fmt.Errorf("unknown type of HostedControlPlane found in namespace %s", namespace)
	}
	return hcp, nil
}

func hyperShiftReplaceNamespaceHook(operandNamespace string) dc.DeploymentHookFunc {
	return func(_ *operatorv1.OperatorSpec, deployment *appsv1.Deployment) error {
		deployment.Namespace = operandNamespace
		return nil
	}
}

func hyperShiftColocationHook(controlPlaneNamespace string) dc.DeploymentHookFunc {
	return func(_ *operatorv1.OperatorSpec, d *appsv1.Deployment) error {
		if d.Spec.Template.Spec.Affinity == nil {
			d.Spec.Template.Spec.Affinity = &corev1.Affinity{}
		}
		if d.Spec.Template.Spec.Affinity.PodAffinity == nil {
			d.Spec.Template.Spec.Affinity.PodAffinity = &corev1.PodAffinity{}
		}
		if d.Spec.Template.Labels == nil {
			d.Spec.Template.Labels = map[string]string{}
		}
		d.Spec.Template.Labels["hypershift.openshift.io/hosted-control-plane"] = controlPlaneNamespace
		d.Spec.Template.Spec.Affinity.PodAffinity.PreferredDuringSchedulingIgnoredDuringExecution = []corev1.WeightedPodAffinityTerm{
			{
				Weight: 100,
				PodAffinityTerm: corev1.PodAffinityTerm{
					LabelSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"hypershift.openshift.io/hosted-control-plane": controlPlaneNamespace,
						},
					},
					TopologyKey: corev1.LabelHostname,
				},
			},
		}
		return nil
	}
}

func hyperShiftControlPlaneIsolationHook(controlPlaneNamespace string) dc.DeploymentHookFunc {
	return func(_ *operatorv1.OperatorSpec, d *appsv1.Deployment) error {
		d.Spec.Template.Spec.Tolerations = []corev1.Toleration{
			{
				Key:      "hypershift.openshift.io/control-plane",
				Operator: corev1.TolerationOpEqual,
				Value:    "true",
				Effect:   corev1.TaintEffectNoSchedule,
			},
			{
				Key:      "hypershift.openshift.io/cluster",
				Operator: corev1.TolerationOpEqual,
				Value:    controlPlaneNamespace,
				Effect:   corev1.TaintEffectNoSchedule,
			},
		}

		if d.Spec.Template.Spec.Affinity == nil {
			d.Spec.Template.Spec.Affinity = &corev1.Affinity{}
		}
		if d.Spec.Template.Spec.Affinity.NodeAffinity == nil {
			d.Spec.Template.Spec.Affinity.NodeAffinity = &corev1.NodeAffinity{}
		}

		d.Spec.Template.Spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution = []corev1.PreferredSchedulingTerm{
			{
				Weight: 50,
				Preference: corev1.NodeSelectorTerm{
					MatchExpressions: []corev1.NodeSelectorRequirement{
						{
							Key:      "hypershift.openshift.io/control-plane",
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"true"},
						},
					},
				},
			},
			{
				Weight: 100,
				Preference: corev1.NodeSelectorTerm{
					MatchExpressions: []corev1.NodeSelectorRequirement{
						{
							Key:      "hypershift.openshift.io/cluster",
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{controlPlaneNamespace},
						},
					},
				},
			},
		}
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

func hyperShiftAddPullSecret() dc.DeploymentHookFunc {
	return func(_ *operatorv1.OperatorSpec, deployment *appsv1.Deployment) error {
		if deployment.Spec.Template.Spec.ImagePullSecrets == nil {
			deployment.Spec.Template.Spec.ImagePullSecrets = []v1.LocalObjectReference{}
		}
		pullSecretRef := v1.LocalObjectReference{
			Name: hyperShiftPullSecretName,
		}
		deployment.Spec.Template.Spec.ImagePullSecrets = append(deployment.Spec.Template.Spec.ImagePullSecrets, pullSecretRef)
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

func withVolumeGroupSnapshotWebhook(enabled bool) dc.DeploymentHookFunc {
	return func(_ *operatorv1.OperatorSpec, deployment *appsv1.Deployment) error {
		if !enabled {
			return nil
		}
		for i := range deployment.Spec.Template.Spec.Containers {
			container := &deployment.Spec.Template.Spec.Containers[i]
			switch container.Name {
			case "snapshot-controller":
				container.Args = append(container.Args, "--enable-volume-group-snapshots")
			case "webhook":
				container.Args = append(container.Args, "--enable-volume-group-snapshot-webhook")
			}
		}
		return nil
	}
}
