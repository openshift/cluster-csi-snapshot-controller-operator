package webhookdeployment

import (
	"context"
	"sort"
	"testing"

	"github.com/google/go-cmp/cmp"
	opv1 "github.com/openshift/api/operator/v1"
	fakeop "github.com/openshift/client-go/operator/clientset/versioned/fake"
	opinformers "github.com/openshift/client-go/operator/informers/externalversions"
	"github.com/openshift/cluster-csi-snapshot-controller-operator/pkg/operatorclient"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	coreinformers "k8s.io/client-go/informers"
	fakecore "k8s.io/client-go/kubernetes/fake"
	core "k8s.io/client-go/testing"
)

type operatorTest struct {
	name            string
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
	csiSnapshotController *opv1.CSISnapshotController
	webhookConfig         *admissionv1.ValidatingWebhookConfiguration
}

const (
	deploymentNamespace = "openshift-cluster-storage-operator"
	webhookName         = "snapshot.storage.k8s.io"
)

var masterNodeLabels = map[string]string{"node-role.kubernetes.io/master": ""}

func newOperator(test operatorTest) *testContext {
	// Convert to []runtime.Object
	var initialObjects []runtime.Object
	if test.initialObjects.webhookConfig != nil {
		initialObjects = append(initialObjects, test.initialObjects.webhookConfig)
	}
	coreClient := fakecore.NewSimpleClientset(initialObjects...)
	coreInformerFactory := coreinformers.NewSharedInformerFactory(coreClient, 0 /*no resync */)
	if test.initialObjects.webhookConfig != nil {
		coreInformerFactory.Admissionregistration().V1().ValidatingWebhookConfigurations().Informer().GetIndexer().Add(test.initialObjects.webhookConfig)
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
		coreInformerFactory.Admissionregistration().V1().ValidatingWebhookConfigurations(),
		coreClient,
		recorder,
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

func withGeneration(generations ...int64) csiSnapshotControllerModifier {
	return func(i *opv1.CSISnapshotController) *opv1.CSISnapshotController {
		i.Generation = generations[0]
		if len(generations) > 1 {
			i.Status.ObservedGeneration = generations[1]
		}
		return i
	}
}

func withOperandGeneration(webhookGeneration int64) csiSnapshotControllerModifier {
	return func(i *opv1.CSISnapshotController) *opv1.CSISnapshotController {
		i.Status.Generations = []opv1.GenerationStatus{
			{
				Group:          admissionv1.GroupName,
				LastGeneration: webhookGeneration,
				Name:           webhookName,
				Namespace:      "",
				Resource:       "validatingwebhookconfigurations",
			},
		}
		return i
	}
}

// ValidatingWebhookConfiguration

type validatingWebhookConfigurationModifier func(*admissionv1.ValidatingWebhookConfiguration) *admissionv1.ValidatingWebhookConfiguration

func validatingWebhookConfiguration(generation int64, modifiers ...validatingWebhookConfigurationModifier) *admissionv1.ValidatingWebhookConfiguration {
	instance, err := getWebhookConfig()
	if err != nil {
		panic(err)
	}
	instance.Generation = generation
	for _, modifier := range modifiers {
		instance = modifier(instance)
	}
	// if any modifiers have been applied to our test configuration, we recalculate the hash
	if len(modifiers) > 0 {
		resourceapply.SetSpecHashAnnotation(&instance.ObjectMeta, instance.Webhooks)
	}
	return instance
}

func withWebhookPort(port int32) validatingWebhookConfigurationModifier {
	return func(instance *admissionv1.ValidatingWebhookConfiguration) *admissionv1.ValidatingWebhookConfiguration {
		for i := range instance.Webhooks {
			instance.Webhooks[i].ClientConfig.Service.Port = &port
		}
		return instance
	}
}

// This reactor is always enabled and bumps Deployment generation when it gets updated.
func addGenerationReactor(client *fakecore.Clientset) {
	client.PrependReactor("*", "validatingwebhookconfigurations", func(action core.Action) (handled bool, ret runtime.Object, err error) {
		switch a := action.(type) {
		case core.CreateActionImpl:
			object := a.GetObject()
			webhook := object.(*admissionv1.ValidatingWebhookConfiguration)
			webhook.Generation++
			return false, webhook, nil
		case core.UpdateActionImpl:
			object := a.GetObject()
			webhook := object.(*admissionv1.ValidatingWebhookConfiguration)
			webhook.Generation++
			return false, webhook, nil
		}
		return false, nil, nil
	})
}

func TestSync(t *testing.T) {
	tests := []operatorTest{
		{
			// Only CSISnapshotController exists, everything else is created
			name: "initial sync",
			initialObjects: testObjects{
				csiSnapshotController: csiSnapshotController(),
			},
			expectedObjects: testObjects{
				webhookConfig: validatingWebhookConfiguration(1),
				csiSnapshotController: csiSnapshotController(
					withOperandGeneration(1),
				),
			},
		},
		{
			// Webhook is updated by user and gets replaced by the operator.
			name: "webhook modified by user",
			initialObjects: testObjects{
				webhookConfig:         validatingWebhookConfiguration(2, withWebhookPort(8080)), // Use changed the port, which changed the generation
				csiSnapshotController: csiSnapshotController(withOperandGeneration(1)),          // the operator knows the old generation of the webhook
			},
			expectedObjects: testObjects{
				webhookConfig: validatingWebhookConfiguration(3), // the webhook port was fixed + generation bumped
				csiSnapshotController: csiSnapshotController(
					withOperandGeneration(3),
				),
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Initialize
			ctx := newOperator(test)

			// Act
			syncContext := factory.NewSyncContext("test", events.NewRecorder(ctx.coreClient.CoreV1().Events("test"), "test-operator", &v1.ObjectReference{}))
			err := ctx.webhookController.Sync(context.TODO(), syncContext)

			// Assert
			// Check error
			if err != nil && !test.expectErr {
				t.Errorf("sync() returned unexpected error: %v", err)
			}
			if err == nil && test.expectErr {
				t.Error("sync() unexpectedly succeeded when error was expected")
			}

			// Check expectedObjects.webhookConfig
			if test.expectedObjects.webhookConfig != nil {
				actualWebhookConfig, err := ctx.coreClient.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(context.TODO(), webhookName, metav1.GetOptions{})
				if err != nil {
					t.Errorf("Failed to get ValidatingWebhookConfiguration %s: %v", webhookName, err)
				}
				sanitizeWebhookConfig(actualWebhookConfig)
				sanitizeWebhookConfig(test.expectedObjects.webhookConfig)
				if !equality.Semantic.DeepEqual(test.expectedObjects.webhookConfig, actualWebhookConfig) {
					t.Errorf("Unexpected ValidatingWebhookConfiguration %+v content:\n%s", webhookName, cmp.Diff(test.expectedObjects.webhookConfig, actualWebhookConfig))
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
					t.Errorf("Unexpected CSISnapshotController content:\n%s", cmp.Diff(test.expectedObjects.csiSnapshotController, actualCSISnapshotController))
				}
			}
		})
	}
}

func sanitizeWebhookConfig(webhookConfig *admissionv1.ValidatingWebhookConfiguration) {
	// nil and empty array are the same
	if len(webhookConfig.Labels) == 0 {
		webhookConfig.Labels = nil
	}
	if len(webhookConfig.Annotations) == 0 {
		webhookConfig.Annotations = nil
	}
	if webhookConfig.Annotations != nil {
		webhookConfig.Annotations["operator.openshift.io/spec-hash"] = ""
	}
	for i := range webhookConfig.Webhooks {
		// port 443 is defaulted
		if webhookConfig.Webhooks[i].ClientConfig.Service.Port == nil {
			var port int32 = 443
			webhookConfig.Webhooks[i].ClientConfig.Service.Port = &port
		}
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
