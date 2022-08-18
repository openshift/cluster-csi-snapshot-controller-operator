package operator

import (
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
	"github.com/openshift/cluster-csi-snapshot-controller-operator/pkg/common"
	"github.com/openshift/cluster-csi-snapshot-controller-operator/pkg/operator/webhookdeployment"
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

	"k8s.io/klog/v2"
)

const (
	targetName        = "csi-snapshot-controller"
	targetNamespace   = "openshift-cluster-storage-operator"
	operatorNamespace = "openshift-cluster-storage-operator"

	operatorVersionEnvName = "OPERATOR_IMAGE_VERSION"
	operandVersionEnvName  = "OPERAND_IMAGE_VERSION"
	operandImageEnvName    = "OPERAND_IMAGE"
	webhookImageEnvName    = "WEBHOOK_IMAGE"

	resync = 20 * time.Minute
)

func RunOperator(ctx context.Context, controllerConfig *controllercmd.ControllerContext) error {
	cb, err := common.NewBuilder("")
	if err != nil {
		klog.Fatalf("error creating clients: %v", err)
	}
	ctrlctx := common.CreateControllerContext(cb, ctx.Done(), targetNamespace)

	configClient, err := configclient.NewForConfig(controllerConfig.KubeConfig)
	if err != nil {
		return err
	}

	configInformers := configinformer.NewSharedInformerFactoryWithOptions(configClient, resync)

	// Create GenericOperatorclient. This is used by the library-go controllers created down below
	gvr := operatorv1.SchemeGroupVersion.WithResource("csisnapshotcontrollers")
	operatorClient, dynamicInformers, err := goc.NewClusterScopedOperatorClientWithConfigName(controllerConfig.KubeConfig, gvr, "cluster")
	if err != nil {
		return err
	}

	kubeClient := ctrlctx.ClientBuilder.KubeClientOrDie(targetName)

	versionGetter := status.NewVersionGetter()

	kubeInformersForNamespaces := v1helpers.NewKubeInformersForNamespaces(kubeClient, operatorNamespace, targetNamespace)
	staticResourcesController := staticresourcecontroller.NewStaticResourceController(
		"CSISnapshotStaticResourceController",
		assets.ReadFile,
		[]string{
			"volumesnapshots.yaml",
			"volumesnapshotcontents.yaml",
			"volumesnapshotclasses.yaml"},
		(&resourceapply.ClientHolder{}).WithKubernetes(kubeClient),
		operatorClient,
		controllerConfig.EventRecorder,
	).WithConditionalResources(
		assets.ReadFile,
		[]string{
			"csi_controller_deployment_pdb.yaml",
			"webhook_deployment_pdb.yaml",
		},
		func() bool {
			isSNO, precheckSucceeded, err := staticcontrollercommon.NewIsSingleNodePlatformFn(configInformers.Config().V1().Infrastructures())()
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
			isSNO, precheckSucceeded, err := staticcontrollercommon.NewIsSingleNodePlatformFn(configInformers.Config().V1().Infrastructures())()
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
	).AddKubeInformers(kubeInformersForNamespaces)

	deploymentManifest, err := assets.ReadFile("csi_controller_deployment.yaml")
	if err != nil {
		return err
	}
	deploymentController := dc.NewDeploymentController(
		"CSISnapshotController",
		deploymentManifest,
		controllerConfig.EventRecorder,
		operatorClient,
		kubeClient,
		ctrlctx.KubeNamespacedInformerFactory.Apps().V1().Deployments(),
		[]factory.Informer{
			ctrlctx.KubeNamespacedInformerFactory.Core().V1().Nodes().Informer(),
			configInformers.Config().V1().Infrastructures().Informer(),
		},
		[]dc.ManifestHookFunc{
			replacePlaceholdersHook(os.Getenv(operandImageEnvName)),
		},
		csidrivercontrollerservicecontroller.WithControlPlaneTopologyHook(configInformers),
		csidrivercontrollerservicecontroller.WithReplicasHook(
			kubeInformersForNamespaces.InformersFor("").Core().V1().Nodes().Lister(),
		),
	)

	versionController := NewVersionController(
		"VersionController",
		operatorClient,
		versionGetter,
		controllerConfig.EventRecorder,
		"CSISnapshotControllerAvailable",
		"CSISnapshotControllerProgressing",
		os.Getenv(operatorVersionEnvName),
		os.Getenv(operandVersionEnvName),
	)

	webhookOperator := webhookdeployment.NewCSISnapshotWebhookController(
		operatorClient,
		ctrlctx.KubeNamespacedInformerFactory.Core().V1().Nodes(),
		ctrlctx.KubeNamespacedInformerFactory.Apps().V1().Deployments(),
		ctrlctx.KubeNamespacedInformerFactory.Admissionregistration().V1().ValidatingWebhookConfigurations(),
		configInformers.Config().V1().Infrastructures(),
		kubeClient,
		controllerConfig.EventRecorder,
		os.Getenv(webhookImageEnvName),
	)

	clusterOperatorStatus := status.NewClusterOperatorStatusController(
		targetName,
		[]configv1.ObjectReference{
			{Resource: "namespaces", Name: targetNamespace},
			{Resource: "namespaces", Name: operatorNamespace},
			{Group: operatorv1.GroupName, Resource: "csisnapshotcontrollers", Name: "cluster"},
		},
		configClient.ConfigV1(),
		configInformers.Config().V1().ClusterOperators(),
		operatorClient,
		versionGetter,
		controllerConfig.EventRecorder,
	)

	logLevelController := loglevel.NewClusterOperatorLoggingController(operatorClient, controllerConfig.EventRecorder)
	managementStateController := managementstatecontroller.NewOperatorManagementStateController(targetName, operatorClient, controllerConfig.EventRecorder)
	management.SetOperatorNotRemovable()

	klog.Info("Starting the Informers.")
	for _, informer := range []interface {
		Start(stopCh <-chan struct{})
	}{
		dynamicInformers,
		configInformers,
		kubeInformersForNamespaces,
		ctrlctx.KubeNamespacedInformerFactory, // operand Deployment
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
		staticResourcesController,
		webhookOperator,
		deploymentController,
		versionController,
	} {
		go controller.Run(ctx, 1)
	}

	<-ctx.Done()

	return fmt.Errorf("stopped")
}

func replacePlaceholdersHook(imageName string) dc.ManifestHookFunc {
	return func(spec *operatorv1.OperatorSpec, manifest []byte) ([]byte, error) {
		pairs := []string{
			"${CONTROLLER_IMAGE}", imageName,
		}
		logLevel := loglevel.LogLevelToVerbosity(spec.LogLevel)
		pairs = append(pairs, "${LOG_LEVEL}", fmt.Sprint(logLevel))

		replaced := strings.NewReplacer(pairs...).Replace(string(manifest))
		return []byte(replaced), nil
	}
}
