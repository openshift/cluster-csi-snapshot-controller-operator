package operator

import (
	"fmt"
	"time"

	apiextv1beta1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/resource/resourceread"

	"github.com/openshift/cluster-csi-snapshot-controller-operator/pkg/generated"
)

var crds = [...]string{"volumesnapshots.yaml",
	"volumesnapshotcontents.yaml",
	"volumesnapshotclasses.yaml"}

const (
	customResourceReadyInterval = time.Second
	customResourceReadyTimeout  = 10 * time.Minute
)

func (c *csiSnapshotOperator) syncCustomResourceDefinitions() error {
	for _, file := range crds {
		crd := resourceread.ReadCustomResourceDefinitionV1Beta1OrDie(generated.MustAsset(file))
		_, updated, err := resourceapply.ApplyCustomResourceDefinition(c.crdClient.ApiextensionsV1beta1(), c.eventRecorder, crd)
		if err != nil {
			return err
		}
		if updated {
			if err := c.waitForCustomResourceDefinition(crd); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *csiSnapshotOperator) waitForCustomResourceDefinition(resource *apiextv1beta1.CustomResourceDefinition) error {
	var lastErr error
	if err := wait.Poll(customResourceReadyInterval, customResourceReadyTimeout, func() (bool, error) {
		crd, err := c.crdLister.Get(resource.Name)
		if err != nil {
			lastErr = fmt.Errorf("error getting CustomResourceDefinition %s: %v", resource.Name, err)
			return false, nil
		}

		for _, condition := range crd.Status.Conditions {
			if condition.Type == apiextv1beta1.Established && condition.Status == apiextv1beta1.ConditionTrue {
				return true, nil
			}
		}
		lastErr = fmt.Errorf("CustomResourceDefinition %s is not ready. conditions: %v", crd.Name, crd.Status.Conditions)
		return false, nil
	}); err != nil {
		if err == wait.ErrWaitTimeout {
			return fmt.Errorf("%v during syncCustomResourceDefinitions: %v", err, lastErr)
		}
		return err
	}
	return nil
}
