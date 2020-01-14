package operator

import (
	"errors"
	"os"

	"monis.app/go/openshift/operator"

	"github.com/openshift/cluster-csi-snapshot-controller-operator/pkg/generated"

	operatorv1 "github.com/openshift/api/operator/v1"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apiextinformersv1beta1 "k8s.io/apiextensions-apiserver/pkg/client/informers/externalversions/apiextensions/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog"

	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/resource/resourceread"
	"github.com/openshift/library-go/pkg/operator/status"

	logf "sigs.k8s.io/controller-runtime/pkg/runtime/log"
)

var log = logf.Log.WithName("csi_snapshot_controller_operator")
var deploymentVersionHashKey = operatorv1.GroupName + "/rvs-hash"
var crds = [...]string{"volumesnapshots.yaml", "volumesnapshotcontents.yaml", "volumesnapshotclasses.yaml"}

const (
	clusterOperatorName       = "csisnapshot"
	targetName                = "csi-snapshot-controller"
	targetNamespace           = "openshift-csi-snapshot-controller"
	targetNameSpaceController = "openshift-csi-controller"
	targetNameOperator        = "openshift-csi-snapshot-controller-operator"
	targetNameController      = "openshift-csi-snapshot-controller"
	globalConfigName          = "cluster"

	operatorSelfName       = "operator"
	operatorVersionEnvName = "OPERATOR_IMAGE_VERSION"
	operandVersionEnvName  = "OPERAND_IMAGE_VERSION"
	operandImageEnvName    = "IMAGE"

	machineConfigNamespace = "openshift-config-managed"
	userConfigNamespace    = "openshift-config"
)

// static environment variables from operator deployment
var (
	csiSnapshotControllerImage = os.Getenv(targetNameController)

	operatorVersion = os.Getenv(operatorVersionEnvName)
)

type csiSnapshotOperator struct {
	client        OperatorClient
	crdInformer   apiextinformersv1beta1.CustomResourceDefinitionInformer
	crdClient     apiextclient.Interface
	versionGetter status.VersionGetter
	recorder      events.Recorder
}

func NewCSISnapshotControllerOperator(
	client OperatorClient,
	crdInformer apiextinformersv1beta1.CustomResourceDefinitionInformer,
	crdClient apiextclient.Interface,
	versionGetter status.VersionGetter,
	recorder events.Recorder,
) operator.Runner {
	csiOperator := &csiSnapshotOperator{
		client:        client,
		crdClient:     crdClient,
		versionGetter: versionGetter,
		recorder:      recorder,
	}

	csiOperator.handleCRDConfig()
	targetNameFilter := operator.FilterByNames(targetName)

	return operator.New("CSISnapshotControllerOperator", csiOperator,
		operator.WithInformer(crdInformer, targetNameFilter))
}

func (c *csiSnapshotOperator) Key() (metav1.Object, error) {
	return c.client.Client.OpenShiftControllerManagers().Get(globalConfigName, metav1.GetOptions{})
}

func (c *csiSnapshotOperator) Sync(obj metav1.Object) error {
	// Watch CRs for:
	// - CSISnapshots
	// - Deployments
	// - CRD
	// - Status?

	// Ensure the CSISnapshotController deployment exists and matches the default
	// If it doesn't exist, create it.
	// If it does exist and doesn't match, overwrite it

	klog.Info("Running Sync")
	c.handleCRDConfig()

	// Update CSISnapshotController.Status
	return errors.New("unsupported function")
}

func (c *csiSnapshotOperator) handleCRDConfig() error {
	// Initialize the Snapshot Controller CRDs
	for _, file := range crds {
		crd := resourceread.ReadCustomResourceDefinitionV1Beta1OrDie(generated.MustAsset(file))
		_, _, err := resourceapply.ApplyCustomResourceDefinition(c.crdClient.ApiextensionsV1beta1(), c.recorder, crd)
		if err != nil {
			return err
		}
	}
	return nil
}
