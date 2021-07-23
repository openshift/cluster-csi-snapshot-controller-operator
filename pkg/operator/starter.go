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
	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"github.com/openshift/library-go/pkg/operator/loglevel"
	"github.com/openshift/library-go/pkg/operator/management"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/staticresourcecontroller"
	"github.com/openshift/library-go/pkg/operator/status"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/klog/v2"
)

const (
	resync = 20 * time.Minute
)

func RunOperator(ctx context.Context, controllerConfig *controllercmd.ControllerContext) error {
	cb, err := common.NewBuilder("")
	if err != nil {
		klog.Fatalf("error creating clients: %v", err)
	}
	ctrlctx := common.CreateControllerContext(cb, ctx.Done(), targetNamespace)

	csiConfigClient, err := csisnapshotconfigclient.NewForConfig(controllerConfig.KubeConfig)
	if err != nil {
		return err
	}

	csiConfigInformers := informer.NewSharedInformerFactoryWithOptions(csiConfigClient, resync,
		informer.WithTweakListOptions(singleNameListOptions(operatorclient.GlobalConfigName)),
	)

	configClient, err := configclient.NewForConfig(controllerConfig.KubeConfig)
	if err != nil {
		return err
	}

	configInformers := configinformer.NewSharedInformerFactoryWithOptions(configClient, resync)

	operatorClient := &operatorclient.OperatorClient{
		Informers: csiConfigInformers,
		Client:    csiConfigClient.OperatorV1(),
		ExpectedConditions: []string{
			operatorv1.OperatorStatusTypeAvailable,
			webhookdeployment.WebhookControllerName + operatorv1.OperatorStatusTypeAvailable,
		},
	}

	kubeClient := ctrlctx.ClientBuilder.KubeClientOrDie(targetName)

	versionGetter := status.NewVersionGetter()

	kubeInformersForNamespaces := v1helpers.NewKubeInformersForNamespaces(kubeClient, operatorNamespace, targetNamespace)
	staticResourcesController := staticresourcecontroller.NewStaticResourceController(
		"CSISnapshotStaticResourceController",
		assets.ReadFile,
		[]string{
			"csi_controller_deployment_pdb.yaml",
			"webhook_deployment_pdb.yaml",
		},
		(&resourceapply.ClientHolder{}).WithKubernetes(kubeClient),
		operatorClient,
		controllerConfig.EventRecorder,
	).AddKubeInformers(kubeInformersForNamespaces)

	operator := NewCSISnapshotControllerOperator(
		*operatorClient,
		ctrlctx.KubeNamespacedInformerFactory.Core().V1().Nodes(),
		ctrlctx.APIExtInformerFactory.Apiextensions().V1().CustomResourceDefinitions(),
		ctrlctx.ClientBuilder.APIExtClientOrDie(targetName),
		ctrlctx.KubeNamespacedInformerFactory.Apps().V1().Deployments(),
		configInformers.Config().V1().Infrastructures().Lister(),
		kubeClient,
		versionGetter,
		controllerConfig.EventRecorder,
		os.Getenv(operatorVersionEnvName),
		os.Getenv(operandVersionEnvName),
		os.Getenv(operandImageEnvName),
	)

	webhookOperator := webhookdeployment.NewCSISnapshotWebhookController(
		*operatorClient,
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
			{Group: operatorv1.GroupName, Resource: "csisnapshotcontrollers", Name: operatorclient.GlobalConfigName},
		},
		configClient.ConfigV1(),
		configInformers.Config().V1().ClusterOperators(),
		operatorClient,
		versionGetter,
		controllerConfig.EventRecorder,
	)

	logLevelController := loglevel.NewClusterOperatorLoggingController(operatorClient, controllerConfig.EventRecorder)
	// TODO remove this controller once we support Removed
	managementStateController := management.NewOperatorManagementStateController(targetName, operatorClient, controllerConfig.EventRecorder)
	management.SetOperatorNotRemovable()

	klog.Info("Starting the Informers.")
	for _, informer := range []interface {
		Start(stopCh <-chan struct{})
	}{
		csiConfigInformers,
		configInformers,
		kubeInformersForNamespaces,
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
	go operator.Run(1, ctx.Done())

	<-ctx.Done()

	return fmt.Errorf("stopped")
}

func singleNameListOptions(name string) func(opts *metav1.ListOptions) {
	return func(opts *metav1.ListOptions) {
		opts.FieldSelector = fields.OneTermEqualSelector("metadata.name", name).String()
	}
}
