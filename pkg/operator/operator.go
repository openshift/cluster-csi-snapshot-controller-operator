package operator

import (
	"errors"
	"os"

	"monis.app/go/openshift/operator"

	"github.com/openshift/cluster-csi-snapshot-controller-operator/pkg/generated"

	operatorv1 "github.com/openshift/api/operator/v1"
	apiextensionsv1beta1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openshift/library-go/pkg/operator/events"
	//"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/resource/resourceread"
	"github.com/openshift/library-go/pkg/operator/status"

	logf "sigs.k8s.io/controller-runtime/pkg/runtime/log"
)

var log = logf.Log.WithName("csi_snapshot_controller_operator")
var deploymentVersionHashKey = operatorv1.GroupName + "/rvs-hash"
var crds = [...]string{"assets/volumesnapshots.yaml", "assets/volumesnapshotcontents.yaml", "assets/volumesnapshotclasses.yaml"}

const (
	clusterOperatorName       = "csisnapshot"
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
	versionGetter status.VersionGetter
	recorder      events.Recorder
}

func NewCSISnapshotControllerOperator(
	client OperatorClient,
	versionGetter status.VersionGetter,
	recorder events.Recorder,
) operator.Runner {
	csiOperator := &csiSnapshotOperator{
		client:        client,
		versionGetter: versionGetter,
		recorder:      recorder,
	}

	return operator.New("CSISnapshotControllerOperator", csiOperator)
}

/*
func (c *csiSnapshotOperator) initCRDs(client client.Client) {
	// Initialize the Snapshot Controller CRDs
	for _, file := range crds {
		crd, err := newCRDForCluster(file)
		_, createdSuccessfully, err := resourceapply.ApplyCustomResourceDefinition(client, c.recorder, crd)

		if !createdSuccessfully || err != nil {
		}
	}
}
*/

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

	// Update CSISnapshotController.Status

	return errors.New("unsupported function")
}

// Attempts to create a new CRD in the cluster
func newCRDForCluster(filename string) (*apiextensionsv1beta1.CustomResourceDefinition, error) {
	return resourceread.ReadCustomResourceDefinitionV1Beta1OrDie(generated.MustAsset(filename)), nil
}
