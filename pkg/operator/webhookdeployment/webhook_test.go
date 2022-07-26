package webhookdeployment

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"github.com/google/go-cmp/cmp"
	configv1 "github.com/openshift/api/config/v1"
	opv1 "github.com/openshift/api/operator/v1"
	fakecfg "github.com/openshift/client-go/config/clientset/versioned/fake"
	cfginformers "github.com/openshift/client-go/config/informers/externalversions"
	fakeop "github.com/openshift/client-go/operator/clientset/versioned/fake"
	opinformers "github.com/openshift/client-go/operator/informers/externalversions"
	"github.com/openshift/cluster-csi-snapshot-controller-operator/assets"
	"github.com/openshift/cluster-csi-snapshot-controller-operator/pkg/operatorclient"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/resource/resourceread"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
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
	nodes                 []*v1.Node
	deployment            *appsv1.Deployment
	webhookConfig         *admissionv1.ValidatingWebhookConfiguration
	csiSnapshotController *opv1.CSISnapshotController
}

const (
	deploymentName      = "csi-snapshot-webhook"
	deploymentNamespace = "openshift-cluster-storage-operator"
	webhookName         = "snapshot.storage.k8s.io"
)

var masterNodeLabels = map[string]string{"node-role.kubernetes.io/master": ""}

func newOperator(test operatorTest) *testContext {
	// Convert to []runtime.Object
	var initialObjects []runtime.Object
	if len(test.initialObjects.nodes) == 0 {
		test.initialObjects.nodes = []*v1.Node{makeNode("A", masterNodeLabels)}
	}
	for _, node := range test.initialObjects.nodes {
		initialObjects = append(initialObjects, node)
	}
	if test.initialObjects.deployment != nil {
		initialObjects = append(initialObjects, test.initialObjects.deployment)
	}
	if test.initialObjects.webhookConfig != nil {
		initialObjects = append(initialObjects, test.initialObjects.webhookConfig)
	}
	coreClient := fakecore.NewSimpleClientset(initialObjects...)
	coreInformerFactory := coreinformers.NewSharedInformerFactory(coreClient, 0 /*no resync */)
	// Fill the informer
	for _, node := range test.initialObjects.nodes {
		coreInformerFactory.Core().V1().Nodes().Informer().GetIndexer().Add(node)
	}
	if test.initialObjects.deployment != nil {
		coreInformerFactory.Apps().V1().Deployments().Informer().GetIndexer().Add(test.initialObjects.deployment)
	}
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

	defaultInfra := &configv1.Infrastructure{ObjectMeta: metav1.ObjectMeta{Name: "cluster"}}
	configClient := fakecfg.NewSimpleClientset(defaultInfra)
	configInformerFactory := cfginformers.NewSharedInformerFactory(configClient, 0)
	configInformerFactory.Config().V1().Infrastructures().Informer().GetIndexer().Add(defaultInfra)

	// Add global reactors
	addGenerationReactor(coreClient)

	recorder := events.NewInMemoryRecorder("operator")
	ctrl := NewCSISnapshotWebhookController(
		client,
		coreInformerFactory.Core().V1().Nodes(),
		coreInformerFactory.Apps().V1().Deployments(),
		coreInformerFactory.Admissionregistration().V1().ValidatingWebhookConfigurations(),
		configInformerFactory.Config().V1().Infrastructures(),
		coreClient,
		recorder,
		test.image,
		"openshift-cluster-storage-operator",
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

func withGenerations(depolymentGeneration, webhookGeneration int64) csiSnapshotControllerModifier {

	return func(i *opv1.CSISnapshotController) *opv1.CSISnapshotController {
		i.Status.Generations = []opv1.GenerationStatus{
			{
				Group:          appsv1.GroupName,
				LastGeneration: depolymentGeneration,
				Name:           deploymentName,
				Namespace:      deploymentNamespace,
				Resource:       "deployments",
			},
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
	depBytes, err := assets.ReadFile(deploymentAsset)
	if err != nil {
		panic(err)
	}
	dep := resourceread.ReadDeploymentV1OrDie(depBytes)
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
	const replica1 = 1
	const defaultImage = "csi-snahpshot-webhook-image"
	var argsLevel2 = []string{"--tls-cert-file=/etc/snapshot-validation-webhook/certs/tls.crt", "--tls-private-key-file=/etc/snapshot-validation-webhook/certs/tls.key", "--v=2", "--port=8443"}
	var argsLevel6 = []string{"--tls-cert-file=/etc/snapshot-validation-webhook/certs/tls.crt", "--tls-private-key-file=/etc/snapshot-validation-webhook/certs/tls.key", "--v=6", "--port=8443"}

	tests := []operatorTest{
		{
			// Only CSISnapshotController exists, everything else is created
			name:  "initial sync",
			image: defaultImage,
			initialObjects: testObjects{
				csiSnapshotController: csiSnapshotController(),
			},
			expectedObjects: testObjects{
				deployment:    getDeployment(argsLevel2, defaultImage, withDeploymentGeneration(1, 0)),
				webhookConfig: validatingWebhookConfiguration(1),
				csiSnapshotController: csiSnapshotController(
					withGenerations(1, 1),
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
				csiSnapshotController: csiSnapshotController(withGenerations(1, 1)),
			},
			expectedObjects: testObjects{
				deployment:    getDeployment(argsLevel2, defaultImage, withDeploymentGeneration(1, 1), withDeploymentStatus(replica1, replica1, replica1)),
				webhookConfig: validatingWebhookConfiguration(1),
				csiSnapshotController: csiSnapshotController(
					withGenerations(1, 1),
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
				webhookConfig:         validatingWebhookConfiguration(1),
				csiSnapshotController: csiSnapshotController(withGenerations(1, 1)), // the operator knows the old generation of the Deployment
			},
			expectedObjects: testObjects{
				deployment: getDeployment(argsLevel2, defaultImage,
					withDeploymentReplicas(1),      // The operator fixed replica count
					withDeploymentGeneration(3, 1), // ... which bumps generation again
					withDeploymentStatus(replica1, replica1, replica1)),
				webhookConfig: validatingWebhookConfiguration(1),
				csiSnapshotController: csiSnapshotController(
					withGenerations(3, 1), // now the operator knows generation 1
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
				webhookConfig: validatingWebhookConfiguration(1),
				csiSnapshotController: csiSnapshotController(
					withGenerations(1, 1),
					withGeneration(1, 1),
					withTrueConditions(opv1.OperatorStatusTypeAvailable),
					withFalseConditions(opv1.OperatorStatusTypeProgressing)),
			},
			expectedObjects: testObjects{
				deployment: getDeployment(argsLevel2, defaultImage,
					withDeploymentGeneration(1, 1),
					withDeploymentStatus(0, 0, 0)), // No change to the Deployment
				webhookConfig: validatingWebhookConfiguration(1),
				csiSnapshotController: csiSnapshotController(
					withGenerations(1, 1),
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
				webhookConfig: validatingWebhookConfiguration(1),
				csiSnapshotController: csiSnapshotController(
					withGenerations(1, 1),
					withGeneration(1, 1),
					withTrueConditions(opv1.OperatorStatusTypeAvailable),
					withFalseConditions(opv1.OperatorStatusTypeProgressing)),
			},
			expectedObjects: testObjects{
				deployment: getDeployment(argsLevel2, defaultImage,
					withDeploymentGeneration(1, 1),
					withDeploymentStatus(1, 1, 0)), // No change to the Deployment
				webhookConfig: validatingWebhookConfiguration(1),
				csiSnapshotController: csiSnapshotController(
					withGenerations(1, 1),
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
				webhookConfig: validatingWebhookConfiguration(1),
				csiSnapshotController: csiSnapshotController(
					withGenerations(1, 1),
					withLogLevel(opv1.Trace), // User changed the log level...
					withGeneration(2, 1)),    //... which caused the Generation to increase
			},
			expectedObjects: testObjects{
				deployment: getDeployment(argsLevel6, defaultImage, // The operator changed cmdline arguments with a new log level
					withDeploymentGeneration(2, 1), // ... which caused the Generation to increase
					withDeploymentStatus(replica1, replica1, replica1)),
				webhookConfig: validatingWebhookConfiguration(1),
				csiSnapshotController: csiSnapshotController(
					withLogLevel(opv1.Trace),
					withGenerations(2, 1),
					withGeneration(2, 1), // Webhook Deployment does not update the CR, only the main controller does so.
					withTrueConditions(opv1.OperatorStatusTypeAvailable, opv1.OperatorStatusTypeProgressing), // Progressing due to Generation change
					withFalseConditions()),
			},
		},
		{
			// Webhook is updated by user and gets replaced by the operator.
			name:  "webhook modified by user",
			image: defaultImage,
			initialObjects: testObjects{
				deployment: getDeployment(argsLevel2, defaultImage,
					withDeploymentReplicas(1),
					withDeploymentGeneration(1, 1),
					withDeploymentStatus(replica1, replica1, replica1)),
				webhookConfig:         validatingWebhookConfiguration(2, withWebhookPort(8080)), // Use changed the port, which changed the generation
				csiSnapshotController: csiSnapshotController(withGenerations(1, 1)),             // the operator knows the old generation of the webhook
			},
			expectedObjects: testObjects{
				deployment: getDeployment(argsLevel2, defaultImage,
					withDeploymentReplicas(1),
					withDeploymentGeneration(1, 1),
					withDeploymentStatus(replica1, replica1, replica1)),
				webhookConfig: validatingWebhookConfiguration(3), // the webhook port was fixed + generation bumped
				csiSnapshotController: csiSnapshotController(
					withGenerations(1, 3),
					withTrueConditions(opv1.OperatorStatusTypeAvailable),
					withFalseConditions(opv1.OperatorStatusTypeProgressing)),
			},
		},
		{
			// Deployment replicas is adjusted according to number of node selector
			name:  "number of replicas is set accordingly",
			image: defaultImage,
			initialObjects: testObjects{
				nodes: []*v1.Node{ // 3 master nodes
					makeNode("A", masterNodeLabels),
					makeNode("B", masterNodeLabels),
					makeNode("C", masterNodeLabels),
				},
				deployment: getDeployment(argsLevel2, defaultImage,
					withDeploymentReplicas(1), // just 1 replica
					withDeploymentGeneration(1, 1),
					withDeploymentStatus(replica1, replica1, replica1)),
				webhookConfig: validatingWebhookConfiguration(1),
				csiSnapshotController: csiSnapshotController(
					withGenerations(1, 1),
					withGeneration(1, 1),
					withTrueConditions(opv1.OperatorStatusTypeAvailable),
					withFalseConditions(opv1.OperatorStatusTypeProgressing)),
			},
			expectedObjects: testObjects{
				deployment: getDeployment(argsLevel2, defaultImage,
					withDeploymentReplicas(2),      // the operator changed the number of replicas to 2
					withDeploymentGeneration(2, 1), // which bumped the generation
					withDeploymentStatus(replica1, replica1, replica1)),
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

func makeNode(suffix string, labels map[string]string) *v1.Node {
	return &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   fmt.Sprintf("node-%s", suffix),
			Labels: labels,
		},
	}
}
