package operator

import (
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apiextv1beta1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/resource/resourceread"

	"github.com/openshift/cluster-csi-snapshot-controller-operator/pkg/generated"
)

var crds = [...]string{"volumesnapshots.yaml",
	"volumesnapshotcontents.yaml",
	"volumesnapshotclasses.yaml"}

var deployment = "csi_controller_deployment.yaml"

const (
	deploymentRolloutPollInterval = time.Second
	deploymentRolloutTimeout      = 10 * time.Minute

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

func (c *csiSnapshotOperator) syncDeployments() error {
	deploy := resourceread.ReadDeploymentV1OrDie(generated.MustAsset(deployment))
	_, updated, err := resourceapply.ApplyDeployment(c.deployClient, c.eventRecorder, deploy, 0, true)
	if err != nil {
		return err
	}
	if updated {
		if err := c.waitForDeploymentRollout(deploy); err != nil {
			return err
		}
	}
	return nil
}

//nolint:dupl
func (c *csiSnapshotOperator) waitForDeploymentRollout(resource *appsv1.Deployment) error {
	var lastErr error
	if err := wait.Poll(deploymentRolloutPollInterval, deploymentRolloutTimeout, func() (bool, error) {
		d, err := c.deployLister.Deployments(resource.Namespace).Get(resource.Name)
		if apierrors.IsNotFound(err) {
			// exit early to recreate the deployment.
			return false, err
		}
		if err != nil {
			// Do not return error here, as we could be updating the API Server itself, in which case we
			// want to continue waiting.
			lastErr = fmt.Errorf("error getting Deployment %s during rollout: %v", resource.Name, err)
			return false, nil
		}

		if d.DeletionTimestamp != nil {
			return false, fmt.Errorf("Deployment %s is being deleted", resource.Name)
		}

		if d.Generation <= d.Status.ObservedGeneration && d.Status.UpdatedReplicas == d.Status.Replicas && d.Status.UnavailableReplicas == 0 {
			return true, nil
		}
		lastErr = fmt.Errorf("Deployment %s is not ready. status: (replicas: %d, updated: %d, ready: %d, unavailable: %d)", d.Name, d.Status.Replicas, d.Status.UpdatedReplicas, d.Status.ReadyReplicas, d.Status.UnavailableReplicas)
		return false, nil
	}); err != nil {
		if err == wait.ErrWaitTimeout {
			return fmt.Errorf("%v during waitForDeploymentRollout: %v", err, lastErr)
		}
		return err
	}
	return nil
}
