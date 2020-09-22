package operator

import (
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/wait"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/cluster-csi-snapshot-controller-operator/pkg/generated"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/resource/resourcemerge"
	"github.com/openshift/library-go/pkg/operator/resource/resourceread"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
)

var crds = [...]string{"volumesnapshots.yaml",
	"volumesnapshotcontents.yaml",
	"volumesnapshotclasses.yaml"}

var deployment = "csi_controller_deployment.yaml"

var (
	// Technically const, but modified by unit tests...
	customResourceReadyInterval = time.Second
	customResourceReadyTimeout  = 10 * time.Minute
)

type AlphaCRDError struct {
	alphaCRDs []string
}

func (a *AlphaCRDError) Error() string {
	return fmt.Sprintf("cluster-csi-snapshot-controller-operator does not support v1alpha1 version of snapshot CRDs %s installed by user or 3rd party controller", strings.Join(a.alphaCRDs, ", "))
}

func (c *csiSnapshotOperator) syncCustomResourceDefinitions() error {
	if err := c.checkAlphaCRDs(); err != nil {
		return err
	}
	for _, file := range crds {
		crd := resourceread.ReadCustomResourceDefinitionV1OrDie(generated.MustAsset(file))
		_, updated, err := resourceapply.ApplyCustomResourceDefinitionV1(c.crdClient.ApiextensionsV1(), c.eventRecorder, crd)
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

func (c *csiSnapshotOperator) waitForCustomResourceDefinition(resource *apiextv1.CustomResourceDefinition) error {
	var lastErr error
	if err := wait.Poll(customResourceReadyInterval, customResourceReadyTimeout, func() (bool, error) {
		crd, err := c.crdLister.Get(resource.Name)
		if err != nil {
			lastErr = fmt.Errorf("error getting CustomResourceDefinition %s: %v", resource.Name, err)
			return false, nil
		}

		for _, condition := range crd.Status.Conditions {
			if condition.Type == apiextv1.Established && condition.Status == apiextv1.ConditionTrue {
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

// checkCRDAlpha checks if v1alpha1 version of the CRD exists and returns human-friendly error if so.
// This happens during update from 4.3 cluster that may use v1alpha1 CRD version installed by cluster admin.
func (c *csiSnapshotOperator) checkAlphaCRDs() error {
	var alphas []string
	for _, file := range crds {
		crd := resourceread.ReadCustomResourceDefinitionV1OrDie(generated.MustAsset(file))
		oldCRD, err := c.crdLister.Get(crd.Name)
		if err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("error getting CustomResourceDefinition %s: %v", crd.Name, err)
		}
		for _, version := range oldCRD.Spec.Versions {
			if version.Name == "v1alpha1" {
				alphas = append(alphas, oldCRD.Name)
			}
		}
	}
	if len(alphas) != 0 {
		return &AlphaCRDError{alphas}
	}
	return nil
}

func (c *csiSnapshotOperator) syncDeployment(instance *operatorv1.CSISnapshotController) (*appsv1.Deployment, error) {
	deploy := c.getExpectedDeployment(instance)

	deploy, _, err := resourceapply.ApplyDeployment(
		c.kubeClient.AppsV1(),
		c.eventRecorder,
		deploy,
		resourcemerge.ExpectedDeploymentGeneration(deploy, instance.Status.Generations))
	if err != nil {
		return nil, err
	}
	return deploy, nil
}

func (c *csiSnapshotOperator) getExpectedDeployment(instance *operatorv1.CSISnapshotController) *appsv1.Deployment {
	deployment := resourceread.ReadDeploymentV1OrDie(generated.MustAsset(deployment))
	deployment.Spec.Template.Spec.Containers[0].Image = c.csiSnapshotControllerImage

	logLevel := getLogLevel(instance.Spec.LogLevel)
	for i, arg := range deployment.Spec.Template.Spec.Containers[0].Args {
		if strings.HasPrefix(arg, "--v=") {
			deployment.Spec.Template.Spec.Containers[0].Args[i] = fmt.Sprintf("--v=%d", logLevel)
		}
	}

	return deployment
}

func getLogLevel(logLevel operatorv1.LogLevel) int {
	switch logLevel {
	case operatorv1.Normal, "":
		return 2
	case operatorv1.Debug:
		return 4
	case operatorv1.Trace:
		return 6
	case operatorv1.TraceAll:
		return 100
	default:
		return 2
	}
}

func (c *csiSnapshotOperator) syncStatus(instance *operatorv1.CSISnapshotController, deployment *appsv1.Deployment) error {
	c.syncConditions(instance, deployment)

	resourcemerge.SetDeploymentGeneration(&instance.Status.Generations, deployment)
	instance.Status.ObservedGeneration = instance.Generation
	if deployment != nil {
		instance.Status.ReadyReplicas = deployment.Status.UpdatedReplicas
	}

	c.setVersion("operator", c.operatorVersion)
	c.setVersion("csi-snapshot-controller", c.operandVersion)

	return nil
}

func (c *csiSnapshotOperator) syncConditions(instance *operatorv1.CSISnapshotController, deployment *appsv1.Deployment) {
	// The operator does not have any prerequisites (at least now)
	v1helpers.SetOperatorCondition(&instance.Status.OperatorStatus.Conditions,
		operatorv1.OperatorCondition{
			Type:   operatorv1.OperatorStatusTypePrereqsSatisfied,
			Status: operatorv1.ConditionTrue,
		})
	// The operator is always upgradeable (at least now)
	v1helpers.SetOperatorCondition(&instance.Status.OperatorStatus.Conditions,
		operatorv1.OperatorCondition{
			Type:   operatorv1.OperatorStatusTypeUpgradeable,
			Status: operatorv1.ConditionTrue,
		})
	c.syncProgressingCondition(instance, deployment)
	c.syncAvailableCondition(deployment, instance)
}

func (c *csiSnapshotOperator) syncAvailableCondition(deployment *appsv1.Deployment, instance *operatorv1.CSISnapshotController) {
	// Available: at least one deployment pod is available, regardless at which version
	if deployment != nil && deployment.Status.AvailableReplicas > 0 {
		v1helpers.SetOperatorCondition(&instance.Status.OperatorStatus.Conditions,
			operatorv1.OperatorCondition{
				Type:   operatorv1.OperatorStatusTypeAvailable,
				Status: operatorv1.ConditionTrue,
			})
	} else {
		v1helpers.SetOperatorCondition(&instance.Status.OperatorStatus.Conditions,
			operatorv1.OperatorCondition{
				Type:    operatorv1.OperatorStatusTypeAvailable,
				Status:  operatorv1.ConditionFalse,
				Message: "Waiting for Deployment to deploy csi-snapshot-controller pods",
				Reason:  "AsExpected",
			})
	}
}

func (c *csiSnapshotOperator) syncProgressingCondition(instance *operatorv1.CSISnapshotController, deployment *appsv1.Deployment) {
	// Progressing: true when Deployment has some work to do
	// (false: when all replicas are updated to the latest release and available)/
	var progressing operatorv1.ConditionStatus
	var progressingMessage string
	var expectedReplicas int32
	if deployment != nil && deployment.Spec.Replicas != nil {
		expectedReplicas = *deployment.Spec.Replicas
	}
	switch {
	case deployment == nil:
		// Not reachable in theory, but better to be on the safe side...
		progressing = operatorv1.ConditionTrue
		progressingMessage = "Waiting for Deployment to be created"

	case deployment.Generation != deployment.Status.ObservedGeneration:
		progressing = operatorv1.ConditionTrue
		progressingMessage = "Waiting for Deployment to act on changes"

	case deployment.Status.UnavailableReplicas > 0:
		progressing = operatorv1.ConditionTrue
		progressingMessage = "Waiting for Deployment to deploy csi-snapshot-controller pods"

	case deployment.Status.UpdatedReplicas < expectedReplicas:
		progressing = operatorv1.ConditionTrue
		progressingMessage = "Waiting for Deployment to update csi-snapshot-controller pods"

	case deployment.Status.AvailableReplicas < expectedReplicas:
		progressing = operatorv1.ConditionTrue
		progressingMessage = "Waiting for Deployment to deploy csi-snapshot-controller pods"

	default:
		progressing = operatorv1.ConditionFalse
	}
	v1helpers.SetOperatorCondition(&instance.Status.OperatorStatus.Conditions,
		operatorv1.OperatorCondition{
			Type:    operatorv1.OperatorStatusTypeProgressing,
			Status:  progressing,
			Message: progressingMessage,
			Reason:  "AsExpected",
		})
}
