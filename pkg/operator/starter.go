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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	kubeclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

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
	controlPlaneDynamicInformers := dynamicinformer.NewDynamicSharedInformerFactory(controlPlaneDynamicClient, 12*time.Hour)

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
	if isHyperShift {
		priorityClass = hypershiftPriorityClass
		deploymentHooks = []dc.DeploymentHookFunc{
			hyperShiftReplaceNamespaceHook(controlPlaneNamespace),
			hyperShiftAddKubeConfigVolume("service-network-admin-kubeconfig"), // TODO: use dedicated secret for Snapshots
			hyperShiftAddPullSecret(),
			hyperShiftControlPlaneIsolationHook(controlPlaneNamespace),
			hyperShiftColocationHook(controlPlaneNamespace),
			hyperShiftNodeSelectorHook(controlPlaneDynamicInformers, controlPlaneNamespace),
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
		eventRecorder,
		guestOperatorClient,
		controlPlaneKubeClient,
		controlPlaneInformersForNamespaces.InformersFor(controlPlaneNamespace).Apps().V1().Deployments(),
		[]factory.Informer{
			guestKubeInformersForNamespaces.InformersFor("").Core().V1().Nodes().Informer(),
			guestConfigInformers.Config().V1().Infrastructures().Informer(),
		},
		[]dc.ManifestHookFunc{
			replacePlaceholdersHook(os.Getenv(operandImageEnvName), priorityClass),
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
		eventRecorder,
		guestOperatorClient,
		controlPlaneKubeClient,
		controlPlaneInformersForNamespaces.InformersFor(controlPlaneNamespace).Apps().V1().Deployments(),
		[]factory.Informer{
			guestKubeInformersForNamespaces.InformersFor("").Core().V1().Nodes().Informer(),
			guestConfigInformers.Config().V1().Infrastructures().Informer(),
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

	return fmt.Errorf("stopped")
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

func hyperShiftNodeSelectorHook(dynamicInformers dynamicinformer.DynamicSharedInformerFactory, controlPlaneNamespace string) dc.DeploymentHookFunc {
	return func(_ *operatorv1.OperatorSpec, d *appsv1.Deployment) error {
		// First we make sure the selector starts empty
		d.Spec.Template.Spec.NodeSelector = map[string]string{}

		// Try to get the node selectors from the HCP resource
		hcpGVR := schema.GroupVersionResource{
			Group:    "hypershift.openshift.io",
			Version:  "v1beta1",
			Resource: "hostedcontrolplanes",
		}
		informer := dynamicInformers.ForResource(hcpGVR)

		// TODO: how to find out the name?
		uncastInstance, err := informer.Lister().Get("hypershift-fbertina")
		if apierrors.IsNotFound(err) {
			// Not Hypershift?
			return nil
		}
		if err != nil {
			return err
		}
		instance := uncastInstance.(*unstructured.Unstructured)

		nodeSelector, exists, err := unstructured.NestedStringMap(instance.UnstructuredContent(), "spec", "nodeSelector")
		if !exists {
			return nil
		}
		if err != nil {
			return err
		}

		d.Spec.Template.Spec.NodeSelector = nodeSelector
		return nil
	}
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
