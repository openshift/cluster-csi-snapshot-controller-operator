package operatorclient

import (
	"context"

	operatorv1 "github.com/openshift/api/operator/v1"
	operatorconfigclient "github.com/openshift/client-go/operator/clientset/versioned/typed/operator/v1"
	operatorclientinformers "github.com/openshift/client-go/operator/informers/externalversions"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
)

const (
	GlobalConfigName = "cluster"
)

type OperatorClient struct {
	Informers          operatorclientinformers.SharedInformerFactory
	Client             operatorconfigclient.CSISnapshotControllersGetter
	ExpectedConditions []string
}

func (c OperatorClient) Informer() cache.SharedIndexInformer {
	return c.Informers.Operator().V1().CSISnapshotControllers().Informer()
}

func (c OperatorClient) GetOperatorState() (*operatorv1.OperatorSpec, *operatorv1.OperatorStatus, string, error) {
	instance, err := c.Informers.Operator().V1().CSISnapshotControllers().Lister().Get(GlobalConfigName)
	if err != nil {
		return nil, nil, "", err
	}

	return &instance.Spec.OperatorSpec, &instance.Status.OperatorStatus, instance.ResourceVersion, nil
}

func (c OperatorClient) GetObjectMeta() (*metav1.ObjectMeta, error) {
	instance, err := c.Informers.Operator().V1().CSISnapshotControllers().Lister().Get(GlobalConfigName)
	if err != nil {
		return nil, err
	}
	return &instance.ObjectMeta, nil
}

func (c OperatorClient) UpdateOperatorSpec(resourceVersion string, spec *operatorv1.OperatorSpec) (*operatorv1.OperatorSpec, string, error) {
	original, err := c.Informers.Operator().V1().CSISnapshotControllers().Lister().Get(GlobalConfigName)
	if err != nil {
		return nil, "", err
	}
	copy := original.DeepCopy()
	copy.ResourceVersion = resourceVersion
	copy.Spec.OperatorSpec = *spec

	ret, err := c.Client.CSISnapshotControllers().Update(context.TODO(), copy, metav1.UpdateOptions{})
	if err != nil {
		return nil, "", err
	}

	return &ret.Spec.OperatorSpec, ret.ResourceVersion, nil
}

func (c OperatorClient) UpdateOperatorStatus(resourceVersion string, status *operatorv1.OperatorStatus) (*operatorv1.OperatorStatus, error) {
	original, err := c.Informers.Operator().V1().CSISnapshotControllers().Lister().Get(GlobalConfigName)
	if err != nil {
		return nil, err
	}
	c.addMissingConditions(status)

	copy := original.DeepCopy()
	copy.ResourceVersion = resourceVersion
	copy.Status.OperatorStatus = *status

	ret, err := c.Client.CSISnapshotControllers().UpdateStatus(context.TODO(), copy, metav1.UpdateOptions{})
	if err != nil {
		return nil, err
	}

	return &ret.Status.OperatorStatus, nil
}

// addMissingConditions adds all conditions that must be present to compute proper OperatorStatus CR.
// Since several controllers run in parallel, we must ensure that whichever controller runs the first sync,
// it must report conditions of the other controllers too.
func (c *OperatorClient) addMissingConditions(status *operatorv1.OperatorStatus) {
	for _, cndType := range c.ExpectedConditions {
		if !c.isConditionSet(status, cndType) {
			cnd := operatorv1.OperatorCondition{
				Type:               cndType,
				Status:             operatorv1.ConditionUnknown,
				LastTransitionTime: metav1.Now(),
				Reason:             "InitalSync",
				Message:            "Waiting for the initial sync of the operator",
			}
			v1helpers.SetOperatorCondition(&status.Conditions, cnd)
		}
	}
}

func (c *OperatorClient) isConditionSet(status *operatorv1.OperatorStatus, cndType string) bool {
	for i := range status.Conditions {
		if status.Conditions[i].Type == cndType {
			return true
		}
	}
	return false
}

func (c OperatorClient) GetOperatorInstance() (*operatorv1.CSISnapshotController, error) {
	instance, err := c.Informers.Operator().V1().CSISnapshotControllers().Lister().Get(GlobalConfigName)
	if err != nil {
		return nil, err
	}
	return instance, nil
}
