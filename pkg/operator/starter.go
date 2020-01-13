package operator

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	csisnapshotconfigclient "github.com/openshift/client-go/operator/clientset/versioned"
	informer "github.com/openshift/client-go/operator/informers/externalversions"
	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"github.com/openshift/library-go/pkg/operator/status"
	"k8s.io/apimachinery/pkg/fields"
)

const (
	resync = 20 * time.Minute
)

func RunOperator(ctx context.Context, controllerConfig *controllercmd.ControllerContext) error {
	configClient, err := csisnapshotconfigclient.NewForConfig(controllerConfig.KubeConfig)
	if err != nil {
		return err
	}

	configInformers := informer.NewSharedInformerFactoryWithOptions(configClient, resync,
		informer.WithTweakListOptions(singleNameListOptions("cluster")),
	)

	operatorClient := &OperatorClient{
		configInformers,
		configClient.OperatorV1(),
	}

	versionGetter := status.NewVersionGetter()

	operator := NewCSISnapshotControllerOperator(
		*operatorClient,
		versionGetter,
		controllerConfig.EventRecorder,
	)

	/*
		clusterOperatorStatus := status.NewClusterOperatorStatusController(
			clusterOperatorName,
			[]configv1.ObjectReference{
				{Resource: "namespaces", Name: targetNamespace},
				{Resource: "namespaces", Name: targetNamespaceOperator},
			},
			operatorClient,
			versionGetter,
			ctx.EventRecorder,
		)

		logLevelController := loglevel.NewClusterOperatorLoggingController(operatorClient, ctx.EventRecorder)
		// TODO remove this controller once we support Removed
		managementStateController := management.NewOperatorManagementStateController(clusterOperatorName, operatorClient, ctx.EventRecorder)
		management.SetOperatorNotRemovable()
	*/

	configInformers.Start(ctx.Done())

	go operator.Run(ctx.Done())

	<-ctx.Done()

	return fmt.Errorf("stopped")
}

func singleNameListOptions(name string) func(opts *metav1.ListOptions) {
	return func(opts *metav1.ListOptions) {
		opts.FieldSelector = fields.OneTermEqualSelector("metadata.name", name).String()
	}
}
