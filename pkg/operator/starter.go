package operator

import (
	"context"
	"fmt"
	"os"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	configclient "github.com/openshift/client-go/config/clientset/versioned"
	configinformer "github.com/openshift/client-go/config/informers/externalversions"
	csisnapshotconfigclient "github.com/openshift/client-go/operator/clientset/versioned"
	informer "github.com/openshift/client-go/operator/informers/externalversions"
	"github.com/openshift/cluster-csi-snapshot-controller-operator/assets"
	"github.com/openshift/cluster-csi-snapshot-controller-operator/pkg/common"
	"github.com/openshift/cluster-csi-snapshot-controller-operator/pkg/operator/webhookdeployment"
	"github.com/openshift/cluster-csi-snapshot-controller-operator/pkg/operatorclient"
	"github.com/openshift/library-go/pkg/config/client"
	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"github.com/openshift/library-go/pkg/operator/loglevel"
	"github.com/openshift/library-go/pkg/operator/management"
	"github.com/openshift/library-go/pkg/operator/managementstatecontroller"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	staticcontrollercommon "github.com/openshift/library-go/pkg/operator/staticpod/controller/common"
	"github.com/openshift/library-go/pkg/operator/staticresourcecontroller"
	"github.com/openshift/library-go/pkg/operator/status"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/klog/v2"
)

const (
	resync = 20 * time.Minute
)

func RunOperator(ctx context.Context, controllerConfig *controllercmd.ControllerContext, guestKubeConfigFile string) error {
	// Kubeconfig received through service account or --kubeconfig arg
	// is for the management cluster.
	managementKubeConfig := controllerConfig.KubeConfig
	managementKubeClient := kubernetes.NewForConfigOrDie(rest.AddUserAgent(managementKubeConfig, targetName))
	operandNamespace := defaultTargetNamespace

	// Guest kubeconfig is the same as the management cluster one unless guestKubeConfigFile is provided
	guestKubeConfig := controllerConfig.KubeConfig
	var err error
	if guestKubeConfigFile != "" {
		guestKubeConfig, err = client.GetKubeConfigOrInClusterConfig(guestKubeConfigFile, nil)
		if err != nil {
			return fmt.Errorf("Failed use guest kubeconfig %s: %s", guestKubeConfigFile, err)
		}
		operandNamespace = controllerConfig.OperatorNamespace
	}
	guestKubeClient := kubernetes.NewForConfigOrDie(rest.AddUserAgent(guestKubeConfig, targetName))

	// Use guest kubeconfig for all objects in the guest cluster.
	// If it's empty, then use $KUBECONFIG (= use the same as managedKubeConfig)
	guestCB, err := common.NewBuilder(guestKubeConfig)
	if err != nil {
		klog.Fatalf("error creating clients: %v", err)
	}
	ctrlctx := common.CreateControllerContext(guestCB, ctx.Done(), operandNamespace)

	// Use guest kubeconfig for the operator CR
	csiConfigClient, err := csisnapshotconfigclient.NewForConfig(guestKubeConfig)
	if err != nil {
		return err
	}

	csiConfigInformers := informer.NewSharedInformerFactoryWithOptions(csiConfigClient, resync,
		informer.WithTweakListOptions(singleNameListOptions(operatorclient.GlobalConfigName)),
	)

	// Use guest config for Infrastructure and ClusterOperator
	configClient, err := configclient.NewForConfig(guestKubeConfig)
	if err != nil {
		return err
	}

	configInformers := configinformer.NewSharedInformerFactoryWithOptions(configClient, resync)

	operatorClient := &operatorclient.OperatorClient{
		Informers: csiConfigInformers,
		Client:    csiConfigClient.OperatorV1(),
		ExpectedConditions: []string{
			conditionName(operatorv1.OperatorStatusTypeAvailable),
			webhookdeployment.WebhookControllerName + operatorv1.OperatorStatusTypeAvailable,
		},
	}

	versionGetter := status.NewVersionGetter()

	guestKubeInformersForNamespaces := v1helpers.NewKubeInformersForNamespaces(guestKubeClient)
	managementKubeInformersForNamespaces := v1helpers.NewKubeInformersForNamespaces(managementKubeClient, operandNamespace)

	// Create PDBs in the management cluster
	// TODO: replace namespace!
	staticResourcesController := staticresourcecontroller.NewStaticResourceController(
		"CSISnapshotStaticResourceController",
		assets.ReadFile,
		[]string{},
		(&resourceapply.ClientHolder{}).WithKubernetes(managementKubeClient),
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
	).AddKubeInformers(guestKubeInformersForNamespaces)

	operator := NewCSISnapshotControllerOperator(
		*operatorClient,
		ctrlctx.KubeNamespacedInformerFactory.Core().V1().Nodes(),
		ctrlctx.APIExtInformerFactory.Apiextensions().V1().CustomResourceDefinitions(),
		ctrlctx.ClientBuilder.APIExtClientOrDie(targetName),
		ctrlctx.KubeNamespacedInformerFactory.Apps().V1().Deployments(),
		configInformers.Config().V1().Infrastructures().Lister(),
		managementKubeClient,
		versionGetter,
		controllerConfig.EventRecorder,
		os.Getenv(operatorVersionEnvName),
		os.Getenv(operandVersionEnvName),
		os.Getenv(operandImageEnvName),
		operandNamespace,
	)

	webhookOperator := webhookdeployment.NewCSISnapshotWebhookController(
		*operatorClient,
		ctrlctx.KubeNamespacedInformerFactory.Core().V1().Nodes(),
		ctrlctx.KubeNamespacedInformerFactory.Apps().V1().Deployments(),
		ctrlctx.KubeNamespacedInformerFactory.Admissionregistration().V1().ValidatingWebhookConfigurations(),
		configInformers.Config().V1().Infrastructures(),
		managementKubeClient,
		controllerConfig.EventRecorder,
		os.Getenv(webhookImageEnvName),
		operandNamespace,
	)

	clusterOperatorStatus := status.NewClusterOperatorStatusController(
		targetName,
		[]configv1.ObjectReference{
			{Resource: "namespaces", Name: operandNamespace},
			{Group: operatorv1.GroupName, Resource: "csisnapshotcontrollers", Name: operatorclient.GlobalConfigName},
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
		csiConfigInformers,
		configInformers,
		guestKubeInformersForNamespaces,
		managementKubeInformersForNamespaces,
		ctrlctx.APIExtInformerFactory,         // CRDs
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
	} {
		go controller.Run(ctx, 1)
	}
	klog.Info("Starting the operator.")
	go operator.Run(ctx, 1)

	<-ctx.Done()

	return fmt.Errorf("stopped")
}

func singleNameListOptions(name string) func(opts *metav1.ListOptions) {
	return func(opts *metav1.ListOptions) {
		opts.FieldSelector = fields.OneTermEqualSelector("metadata.name", name).String()
	}
}
