package webhookdeployment

import (
	"context"
	"sort"
	"testing"

	"github.com/google/go-cmp/cmp"
	opv1 "github.com/openshift/api/operator/v1"
	fakeop "github.com/openshift/client-go/operator/clientset/versioned/fake"
	opinformers "github.com/openshift/client-go/operator/informers/externalversions"
	"github.com/openshift/cluster-csi-snapshot-controller-operator/pkg/generated"
	"github.com/openshift/cluster-csi-snapshot-controller-operator/pkg/operatorclient"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/resource/resourceread"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	coreinformers "k8s.io/client-go/informers"
	fakecore "k8s.io/client-go/kubernetes/fake"
	core "k8s.io/client-go/testing"
)

type operatorTest struct {
	name            string
	image           string
	initialObjects  testObjects
	expectedObjects testObjects
	expectErr       bool
}

type testContext struct {
	coreClient        *fakecore.Clientset
	coreInformers     coreinformers.SharedInformerFactory
	operatorClient    *fakeop.Clientset
	operatorInformers opinformers.SharedInformerFactory
	webhookController factory.Controller
}

type testObjects struct {
	deployment            *appsv1.Deployment
	csiSnapshotController *opv1.CSISnapshotController
}

const (
	deploymentName      = "csi-snapshot-webhook"
	deploymentNamespace = "openshift-cluster-storage-operator"
)

func newOperator(test operatorTest) *testContext {
	// Convert to []runtime.Object
	var initialDeployments []runtime.Object
	if test.initialObjects.deployment != nil {
		initialDeployments = []runtime.Object{test.initialObjects.deployment}
	}
	coreClient := fakecore.NewSimpleClientset(initialDeployments...)
	coreInformerFactory := coreinformers.NewSharedInformerFactory(coreClient, 0 /*no resync */)
	// Fill the informer
	if test.initialObjects.deployment != nil {
		coreInformerFactory.Apps().V1().Deployments().Informer().GetIndexer().Add(test.initialObjects.deployment)
	}

	// Convert to []runtime.Object
	var initialCSISnapshotControllers []runtime.Object
	if test.initialObjects.csiSnapshotController != nil {
		initialCSISnapshotControllers = []runtime.Object{test.initialObjects.csiSnapshotController}
	}
	operatorClient := fakeop.NewSimpleClientset(initialCSISnapshotControllers...)
	operatorInformerFactory := opinformers.NewSharedInformerFactory(operatorClient, 0)
	// Fill the informer
	if test.initialObjects.csiSnapshotController != nil {
		operatorInformerFactory.Operator().V1().CSISnapshotControllers().Informer().GetIndexer().Add(test.initialObjects.csiSnapshotController)
	}

	client := operatorclient.OperatorClient{
		Client:    operatorClient.OperatorV1(),
		Informers: operatorInformerFactory,
	}

	// Add global reactors
	addGenerationReactor(coreClient)

	recorder := events.NewInMemoryRecorder("operator")
	ctrl := NewCSISnapshotWebhookController(
		client,
		coreInformerFactory.Apps().V1().Deployments(),
		coreClient,
		recorder,
		test.image,
	)

	return &testContext{
		webhookController: ctrl,
		coreClient:        coreClient,
		coreInformers:     coreInformerFactory,
		operatorClient:    operatorClient,
		operatorInformers: operatorInformerFactory,
	}
}

// CSISnapshotControllers

type csiSnapshotControllerModifier func(*opv1.CSISnapshotController) *opv1.CSISnapshotController

func csiSnapshotController(modifiers ...csiSnapshotControllerModifier) *opv1.CSISnapshotController {
	instance := &opv1.CSISnapshotController{
		TypeMeta: metav1.TypeMeta{APIVersion: opv1.SchemeGroupVersion.String()},
		ObjectMeta: metav1.ObjectMeta{
			Name:       operatorclient.GlobalConfigName,
			Generation: 0,
		},
		Spec: opv1.CSISnapshotControllerSpec{
			OperatorSpec: opv1.OperatorSpec{
				ManagementState: opv1.Managed,
			},
		},
		Status: opv1.CSISnapshotControllerStatus{},
	}
	for _, modifier := range modifiers {
		instance = modifier(instance)
	}
	return instance
}

func withLogLevel(logLevel opv1.LogLevel) csiSnapshotControllerModifier {
	return func(i *opv1.CSISnapshotController) *opv1.CSISnapshotController {
		i.Spec.LogLevel = logLevel
		return i
	}
}

func withGeneration(generations ...int64) csiSnapshotControllerModifier {
	return func(i *opv1.CSISnapshotController) *opv1.CSISnapshotController {
		i.Generation = generations[0]
		if len(generations) > 1 {
			i.Status.ObservedGeneration = generations[1]
		}
		return i
	}
}

func withGenerations(depolymentGeneration int64) csiSnapshotControllerModifier {

	return func(i *opv1.CSISnapshotController) *opv1.CSISnapshotController {
		i.Status.Generations = []opv1.GenerationStatus{
			{
				Group:          appsv1.GroupName,
				LastGeneration: depolymentGeneration,
				Name:           deploymentName,
				Namespace:      deploymentNamespace,
				Resource:       "deployments",
			},
		}
		return i
	}
}

func withTrueConditions(conditions ...string) csiSnapshotControllerModifier {
	return func(i *opv1.CSISnapshotController) *opv1.CSISnapshotController {
		if i.Status.Conditions == nil {
			i.Status.Conditions = []opv1.OperatorCondition{}
		}
		for _, c := range conditions {
			i.Status.Conditions = append(i.Status.Conditions, opv1.OperatorCondition{
				Type:   WebhookControllerName + c,
				Status: opv1.ConditionTrue,
			})
		}
		return i
	}
}

func withFalseConditions(conditions ...string) csiSnapshotControllerModifier {
	return func(i *opv1.CSISnapshotController) *opv1.CSISnapshotController {
		if i.Status.Conditions == nil {
			i.Status.Conditions = []opv1.OperatorCondition{}
		}
		for _, c := range conditions {
			i.Status.Conditions = append(i.Status.Conditions, opv1.OperatorCondition{
				Type:   WebhookControllerName + c,
				Status: opv1.ConditionFalse,
			})
		}
		return i
	}
}

// Deployments

type deploymentModifier func(*appsv1.Deployment) *appsv1.Deployment

func getDeployment(args []string, image string, modifiers ...deploymentModifier) *appsv1.Deployment {
	dep := resourceread.ReadDeploymentV1OrDie(generated.MustAsset(deploymentAsset))
	dep.Spec.Template.Spec.Containers[0].Args = args
	dep.Spec.Template.Spec.Containers[0].Image = image
	var one int32 = 1
	dep.Spec.Replicas = &one

	for _, modifier := range modifiers {
		dep = modifier(dep)
	}

	// Set by ApplyDeployment()
	if dep.Annotations == nil {
		dep.Annotations = map[string]string{}
	}
	resourceapply.SetSpecHashAnnotation(&dep.ObjectMeta, dep.Spec)

	return dep
}

func withDeploymentStatus(readyReplicas, availableReplicas, updatedReplicas int32) deploymentModifier {
	return func(instance *appsv1.Deployment) *appsv1.Deployment {
		instance.Status.ReadyReplicas = readyReplicas
		instance.Status.AvailableReplicas = availableReplicas
		instance.Status.UpdatedReplicas = updatedReplicas
		return instance
	}
}

func withDeploymentReplicas(replicas int32) deploymentModifier {
	return func(instance *appsv1.Deployment) *appsv1.Deployment {
		instance.Spec.Replicas = &replicas
		return instance
	}
}

func withDeploymentGeneration(generations ...int64) deploymentModifier {
	return func(instance *appsv1.Deployment) *appsv1.Deployment {
		instance.Generation = generations[0]
		if len(generations) > 1 {
			instance.Status.ObservedGeneration = generations[1]
		}
		return instance
	}
}

// This reactor is always enabled and bumps Deployment generation when it gets updated.
func addGenerationReactor(client *fakecore.Clientset) {
	client.PrependReactor("*", "deployments", func(action core.Action) (handled bool, ret runtime.Object, err error) {
		switch a := action.(type) {
		case core.CreateActionImpl:
			object := a.GetObject()
			deployment := object.(*appsv1.Deployment)
			deployment.Generation++
			return false, deployment, nil
		case core.UpdateActionImpl:
			object := a.GetObject()
			deployment := object.(*appsv1.Deployment)
			deployment.Generation++
			return false, deployment, nil
		}
		return false, nil, nil
	})
}

func TestSync(t *testing.T) {
	const replica1 = 1
	const defaultImage = "csi-snahpshot-webhook-image"
	var argsLevel2 = []string{"--tls-cert-file=/etc/snapshot-validation-webhook/certs/tls.crt", "--tls-private-key-file=/etc/snapshot-validation-webhook/certs/tls.key", "--v=2"}
	var argsLevel6 = []string{"--tls-cert-file=/etc/snapshot-validation-webhook/certs/tls.crt", "--tls-private-key-file=/etc/snapshot-validation-webhook/certs/tls.key", "--v=6"}

	tests := []operatorTest{
		{
			// Only CSISnapshotController exists, everything else is created
			name:  "initial sync",
			image: defaultImage,
			initialObjects: testObjects{
				csiSnapshotController: csiSnapshotController(),
			},
			expectedObjects: testObjects{
				deployment: getDeployment(argsLevel2, defaultImage, withDeploymentGeneration(1, 0)),
				csiSnapshotController: csiSnapshotController(
					withGenerations(1),
					withTrueConditions(opv1.OperatorStatusTypeProgressing),
					withFalseConditions(opv1.OperatorStatusTypeAvailable)),
			},
		},
		{
			// Deployment is fully deployed and its status is synced to CSISnapshotController
			name:  "deployment fully deployed",
			image: defaultImage,
			initialObjects: testObjects{
				deployment:            getDeployment(argsLevel2, defaultImage, withDeploymentGeneration(1, 1), withDeploymentStatus(replica1, replica1, replica1)),
				csiSnapshotController: csiSnapshotController(withGenerations(1)),
			},
			expectedObjects: testObjects{
				deployment: getDeployment(argsLevel2, defaultImage, withDeploymentGeneration(1, 1), withDeploymentStatus(replica1, replica1, replica1)),
				csiSnapshotController: csiSnapshotController(
					withGenerations(1),
					withTrueConditions(opv1.OperatorStatusTypeAvailable),
					withFalseConditions(opv1.OperatorStatusTypeProgressing)),
			},
		},
		{
			// Deployment has wrong nr. of replicas, modified by user, and gets replaced by the operator.
			name:  "deployment modified by user",
			image: defaultImage,
			initialObjects: testObjects{
				deployment: getDeployment(argsLevel2, defaultImage,
					withDeploymentReplicas(2),      // User changed replicas
					withDeploymentGeneration(2, 1), // ... which changed Generation
					withDeploymentStatus(replica1, replica1, replica1)),
				csiSnapshotController: csiSnapshotController(withGenerations(1)), // the operator knows the old generation of the Deployment
			},
			expectedObjects: testObjects{
				deployment: getDeployment(argsLevel2, defaultImage,
					withDeploymentReplicas(1),      // The operator fixed replica count
					withDeploymentGeneration(3, 1), // ... which bumps generation again
					withDeploymentStatus(replica1, replica1, replica1)),
				csiSnapshotController: csiSnapshotController(
					withGenerations(3), // now the operator knows generation 1
					withTrueConditions(opv1.OperatorStatusTypeAvailable, opv1.OperatorStatusTypeProgressing), // Progressing due to Generation change
					withFalseConditions()),
			},
		},
		{
			// Deployment gets degraded from some reason
			name:  "deployment degraded",
			image: defaultImage,
			initialObjects: testObjects{
				deployment: getDeployment(argsLevel2, defaultImage,
					withDeploymentGeneration(1, 1),
					withDeploymentStatus(0, 0, 0)), // the Deployment has no pods
				csiSnapshotController: csiSnapshotController(
					withGenerations(1),
					withGeneration(1, 1),
					withTrueConditions(opv1.OperatorStatusTypeAvailable),
					withFalseConditions(opv1.OperatorStatusTypeProgressing)),
			},
			expectedObjects: testObjects{
				deployment: getDeployment(argsLevel2, defaultImage,
					withDeploymentGeneration(1, 1),
					withDeploymentStatus(0, 0, 0)), // No change to the Deployment
				csiSnapshotController: csiSnapshotController(
					withGenerations(1),
					withGeneration(1, 1),
					withTrueConditions(opv1.OperatorStatusTypeProgressing), // The operator is Progressing
					withFalseConditions(opv1.OperatorStatusTypeAvailable)), // The operator is not Available (no replica is running...)
			},
		},
		{
			// Deployment is updating pods
			name:  "update",
			image: defaultImage,
			initialObjects: testObjects{
				deployment: getDeployment(argsLevel2, defaultImage,
					withDeploymentGeneration(1, 1),
					withDeploymentStatus(1 /*ready*/, 1 /*available*/, 0 /*updated*/)), // the Deployment is updating 1 pod
				csiSnapshotController: csiSnapshotController(
					withGenerations(1),
					withGeneration(1, 1),
					withTrueConditions(opv1.OperatorStatusTypeAvailable),
					withFalseConditions(opv1.OperatorStatusTypeProgressing)),
			},
			expectedObjects: testObjects{
				deployment: getDeployment(argsLevel2, defaultImage,
					withDeploymentGeneration(1, 1),
					withDeploymentStatus(1, 1, 0)), // No change to the Deployment
				csiSnapshotController: csiSnapshotController(
					withGenerations(1),
					withGeneration(1, 1),
					withTrueConditions(opv1.OperatorStatusTypeAvailable, opv1.OperatorStatusTypeProgressing), // The operator is Progressing, but still Available
					withFalseConditions()),
			},
		},
		{
			// User changes log level and it's projected into the Deployment
			name:  "log level change",
			image: defaultImage,
			initialObjects: testObjects{
				deployment: getDeployment(argsLevel2, defaultImage,
					withDeploymentGeneration(1, 1),
					withDeploymentStatus(replica1, replica1, replica1)),
				csiSnapshotController: csiSnapshotController(
					withGenerations(1),
					withLogLevel(opv1.Trace), // User changed the log level...
					withGeneration(2, 1)),    //... which caused the Generation to increase
			},
			expectedObjects: testObjects{
				deployment: getDeployment(argsLevel6, defaultImage, // The operator changed cmdline arguments with a new log level
					withDeploymentGeneration(2, 1), // ... which caused the Generation to increase
					withDeploymentStatus(replica1, replica1, replica1)),
				csiSnapshotController: csiSnapshotController(
					withLogLevel(opv1.Trace),
					withGenerations(2),
					withGeneration(2, 1), // Webhook Deployment does not update the CR, only the main controller does so.
					withTrueConditions(opv1.OperatorStatusTypeAvailable, opv1.OperatorStatusTypeProgressing), // Progressing due to Generation change
					withFalseConditions()),
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Initialize
			ctx := newOperator(test)

			// Act
			syncContext := factory.NewSyncContext("test", events.NewRecorder(ctx.coreClient.CoreV1().Events("test"), "test-operator", &corev1.ObjectReference{}))
			err := ctx.webhookController.Sync(context.TODO(), syncContext)

			// Assert
			// Check error
			if err != nil && !test.expectErr {
				t.Errorf("sync() returned unexpected error: %v", err)
			}
			if err == nil && test.expectErr {
				t.Error("sync() unexpectedly succeeded when error was expected")
			}

			// Check expectedObjects.deployment
			if test.expectedObjects.deployment != nil {
				actualDeployment, err := ctx.coreClient.AppsV1().Deployments(deploymentNamespace).Get(context.TODO(), deploymentName, metav1.GetOptions{})
				if err != nil {
					t.Errorf("Failed to get Deployment %s: %v", deploymentName, err)
				}
				sanitizeDeployment(actualDeployment)
				sanitizeDeployment(test.expectedObjects.deployment)
				if !equality.Semantic.DeepEqual(test.expectedObjects.deployment, actualDeployment) {
					t.Errorf("Unexpected Deployment %+v content:\n%s", deploymentName, cmp.Diff(test.expectedObjects.deployment, actualDeployment))
				}
			}
			// Check expectedObjects.csiSnapshotController
			if test.expectedObjects.csiSnapshotController != nil {
				actualCSISnapshotController, err := ctx.operatorClient.OperatorV1().CSISnapshotControllers().Get(context.TODO(), operatorclient.GlobalConfigName, metav1.GetOptions{})
				if err != nil {
					t.Errorf("Failed to get CSISnapshotController %s: %v", operatorclient.GlobalConfigName, err)
				}
				sanitizeCSISnapshotController(actualCSISnapshotController)
				sanitizeCSISnapshotController(test.expectedObjects.csiSnapshotController)
				if !equality.Semantic.DeepEqual(test.expectedObjects.csiSnapshotController, actualCSISnapshotController) {
					t.Errorf("Unexpected CSISnapshotController %+v content:\n%s", deploymentName, cmp.Diff(test.expectedObjects.csiSnapshotController, actualCSISnapshotController))
				}
			}
		})
	}
}

func sanitizeDeployment(deployment *appsv1.Deployment) {
	// nil and empty array are the same
	if len(deployment.Labels) == 0 {
		deployment.Labels = nil
	}
	if len(deployment.Annotations) == 0 {
		deployment.Annotations = nil
	}
	if deployment.Annotations != nil {
		deployment.Annotations["operator.openshift.io/spec-hash"] = ""
	}
}

func sanitizeCSISnapshotController(instance *opv1.CSISnapshotController) {
	// Remove condition texts
	for i := range instance.Status.Conditions {
		instance.Status.Conditions[i].LastTransitionTime = metav1.Time{}
		instance.Status.Conditions[i].Message = ""
		instance.Status.Conditions[i].Reason = ""
	}
	// Sort the conditions by name to have consistent position in the array
	sort.Slice(instance.Status.Conditions, func(i, j int) bool {
		return instance.Status.Conditions[i].Type < instance.Status.Conditions[j].Type
	})
}
