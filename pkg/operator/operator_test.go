package operator

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	opv1 "github.com/openshift/api/operator/v1"
	fakeop "github.com/openshift/client-go/operator/clientset/versioned/fake"
	opinformers "github.com/openshift/client-go/operator/informers/externalversions"
	"github.com/openshift/cluster-csi-snapshot-controller-operator/pkg/generated"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/resource/resourceread"
	"github.com/openshift/library-go/pkg/operator/status"
	appsv1 "k8s.io/api/apps/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	fakeextapi "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	apiextinformers "k8s.io/apiextensions-apiserver/pkg/client/informers/externalversions"
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
	reactors        testReactors
	expectErr       bool
}

type testContext struct {
	operator          *csiSnapshotOperator
	coreClient        *fakecore.Clientset
	coreInformers     coreinformers.SharedInformerFactory
	extAPIClient      *fakeextapi.Clientset
	extAPIInformers   apiextinformers.SharedInformerFactory
	operatorClient    *fakeop.Clientset
	operatorInformers opinformers.SharedInformerFactory
}

type testObjects struct {
	deployment            *appsv1.Deployment
	crds                  []*apiextv1.CustomResourceDefinition
	csiSnapshotController *opv1.CSISnapshotController
}

type addCoreReactors func(*fakecore.Clientset, coreinformers.SharedInformerFactory)
type addExtAPIReactors func(*fakeextapi.Clientset, apiextinformers.SharedInformerFactory)
type addOperatorReactors func(*fakeop.Clientset, opinformers.SharedInformerFactory)

type testReactors struct {
	deployments            addCoreReactors
	crds                   addExtAPIReactors
	csiSnapshotControllers addOperatorReactors
}

const testVersion = "0.0.1" // Version of the operator for testing purposes (instead of getenv)

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
	if test.reactors.deployments != nil {
		test.reactors.deployments(coreClient, coreInformerFactory)
	}

	// Convert to []runtime.Object
	initialCRDs := make([]runtime.Object, len(test.initialObjects.crds))
	for i := range test.initialObjects.crds {
		initialCRDs[i] = test.initialObjects.crds[i]
	}
	extAPIClient := fakeextapi.NewSimpleClientset(initialCRDs...)
	extAPIInformerFactory := apiextinformers.NewSharedInformerFactory(extAPIClient, 0)
	// Fill the informer
	for i := range test.initialObjects.crds {
		extAPIInformerFactory.Apiextensions().V1().CustomResourceDefinitions().Informer().GetIndexer().Add(test.initialObjects.crds[i])
	}
	if test.reactors.crds != nil {
		test.reactors.crds(extAPIClient, extAPIInformerFactory)
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
	if test.reactors.csiSnapshotControllers != nil {
		test.reactors.csiSnapshotControllers(operatorClient, operatorInformerFactory)
	}

	// Add global reactors
	addGenerationReactor(coreClient)

	client := OperatorClient{
		Client:    operatorClient.OperatorV1(),
		Informers: operatorInformerFactory,
	}

	versionGetter := status.NewVersionGetter()
	versionGetter.SetVersion("operator", testVersion)
	versionGetter.SetVersion("csi-snapshot-controller", testVersion)

	recorder := events.NewInMemoryRecorder("operator")
	op := NewCSISnapshotControllerOperator(client,
		extAPIInformerFactory.Apiextensions().V1().CustomResourceDefinitions(),
		extAPIClient,
		coreInformerFactory.Apps().V1().Deployments(),
		coreClient,
		versionGetter,
		recorder,
		testVersion,
		testVersion,
		test.image,
	)

	return &testContext{
		operator:          op,
		coreClient:        coreClient,
		coreInformers:     coreInformerFactory,
		extAPIClient:      extAPIClient,
		extAPIInformers:   extAPIInformerFactory,
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
			Name:       "cluster",
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

func withStatus(readyReplicas int32) csiSnapshotControllerModifier {
	return func(i *opv1.CSISnapshotController) *opv1.CSISnapshotController {
		i.Status = opv1.CSISnapshotControllerStatus{
			OperatorStatus: opv1.OperatorStatus{
				ReadyReplicas: readyReplicas,
			},
		}
		return i
	}
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
				Name:           targetName,
				Namespace:      targetNamespace,
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
				Type:   c,
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
				Type:   c,
				Status: opv1.ConditionFalse,
			})
		}
		return i
	}
}

// Deployments

type deploymentModifier func(*appsv1.Deployment) *appsv1.Deployment

func getDeployment(args []string, image string, modifiers ...deploymentModifier) *appsv1.Deployment {
	dep := resourceread.ReadDeploymentV1OrDie(generated.MustAsset(deployment))
	dep.Spec.Template.Spec.Containers[0].Args = args
	dep.Spec.Template.Spec.Containers[0].Image = image
	var one int32 = 1
	dep.Spec.Replicas = &one

	// Set by ApplyDeployment()
	if dep.Annotations == nil {
		dep.Annotations = map[string]string{}
	}
	resourceapply.SetSpecHashAnnotation(&dep.ObjectMeta, dep.Spec)

	for _, modifier := range modifiers {
		dep = modifier(dep)
	}

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

// CRDs

type crdModifier func(*apiextv1.CustomResourceDefinition) *apiextv1.CustomResourceDefinition

func getCRDs(modifiers ...crdModifier) []*apiextv1.CustomResourceDefinition {
	crdObjects := make([]*apiextv1.CustomResourceDefinition, 3)
	for i, file := range crds {
		crd := resourceread.ReadCustomResourceDefinitionV1OrDie(generated.MustAsset(file))
		for _, modifier := range modifiers {
			crd = modifier(crd)
		}
		crdObjects[i] = crd
	}
	return crdObjects
}

func withEstablishedConditions(instance *apiextv1.CustomResourceDefinition) *apiextv1.CustomResourceDefinition {
	instance.Status.Conditions = []apiextv1.CustomResourceDefinitionCondition{
		{
			Type:   apiextv1.Established,
			Status: apiextv1.ConditionTrue,
		},
	}
	return instance
}

func getAlphaCRD(crdName string) *apiextv1.CustomResourceDefinition {
	var crdFile string
	switch crdName {
	case "VolumeSnapshot":
		crdFile = `
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
    name: volumesnapshots.snapshot.storage.k8s.io
spec:
    conversion:
      strategy: None
    group: snapshot.storage.k8s.io
    names:
      kind: VolumeSnapshot
      listKind: VolumeSnapshotList
      plural: volumesnapshots
      singular: volumesnapshot
    preserveUnknownFields: true
    scope: Namespaced
    versions:
    - name: v1alpha1
      served: true
      storage: true
    subresources:
      status: {}
`

	case "VolumeSnapshotContent":
		crdFile = `
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
    name: volumesnapshotcontents.snapshot.storage.k8s.io
spec:
    conversion:
      strategy: None
    group: snapshot.storage.k8s.io
    names:
      kind: VolumeSnapshotContent
      listKind: VolumeSnapshotContentList
      plural: volumesnapshotcontents
      singular: volumesnapshotcontent
    preserveUnknownFields: true
    scope: Cluster
    versions:
    - name: v1alpha1
      served: true
      storage: true
`
	case "VolumeSnapshotClass":
		crdFile = `
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
    name: volumesnapshotclasses.snapshot.storage.k8s.io
spec:
    conversion:
      strategy: None
    group: snapshot.storage.k8s.io
    names:
      kind: VolumeSnapshotClass
      listKind: VolumeSnapshotClassList
      plural: volumesnapshotclasses
      singular: volumesnapshotclass
    preserveUnknownFields: true
    scope: Cluster
    versions:
    - name: v1alpha1
      served: true
      storage: true
    subresources:
      status:
`
	default:
		panic(crdName)
	}
	return resourceread.ReadCustomResourceDefinitionV1OrDie([]byte(crdFile))
}

// Optional reactor that sets Established condition to True. It's needed by the operator that polls for CRDs until they get the condition
func addCRDEstablishedRector(client *fakeextapi.Clientset, informer apiextinformers.SharedInformerFactory) {
	client.PrependReactor("*", "customresourcedefinitions", func(action core.Action) (handled bool, ret runtime.Object, err error) {
		switch a := action.(type) {
		case core.CreateActionImpl:
			object := a.GetObject()
			crd := object.(*apiextv1.CustomResourceDefinition)
			crd = crd.DeepCopy()
			crd = withEstablishedConditions(crd)
			informer.Apiextensions().V1().CustomResourceDefinitions().Informer().GetIndexer().Add(crd)
			return false, crd, nil
		case core.UpdateActionImpl:
			object := a.GetObject()
			crd := object.(*apiextv1.CustomResourceDefinition)
			crd = crd.DeepCopy()
			crd = withEstablishedConditions(crd)
			informer.Apiextensions().V1().CustomResourceDefinitions().Informer().GetIndexer().Update(crd)
			return false, crd, nil
		}
		return false, nil, nil
	})
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
	const replica0 = 0
	const replica1 = 1
	const replica2 = 2
	const defaultImage = "csi-snapshot-controller-image"
	var argsLevel2 = []string{"--v=2", "--leader-election=true"}
	var argsLevel6 = []string{"--v=6", "--leader-election=true"}

	// Override default timeouts to speed up tests
	customResourceReadyInterval = 10 * time.Millisecond
	customResourceReadyTimeout = 1 * time.Second

	tests := []operatorTest{
		{
			// Only CSISnapshotController exists, everything else is created
			name:  "initial sync",
			image: defaultImage,
			initialObjects: testObjects{
				csiSnapshotController: csiSnapshotController(),
			},
			expectedObjects: testObjects{
				crds:       getCRDs(),
				deployment: getDeployment(argsLevel2, defaultImage, withDeploymentGeneration(1, 0)),
				csiSnapshotController: csiSnapshotController(
					withStatus(replica0),
					withGenerations(1),
					withTrueConditions(opv1.OperatorStatusTypeUpgradeable, opv1.OperatorStatusTypePrereqsSatisfied, opv1.OperatorStatusTypeProgressing),
					withFalseConditions(opv1.OperatorStatusTypeDegraded, opv1.OperatorStatusTypeAvailable)),
			},
			reactors: testReactors{
				crds: addCRDEstablishedRector,
			},
		},
		{
			// Deployment is fully deployed and its status is synced to CSISnapshotController
			name:  "deployment fully deployed",
			image: defaultImage,
			initialObjects: testObjects{
				crds:                  getCRDs(withEstablishedConditions),
				deployment:            getDeployment(argsLevel2, defaultImage, withDeploymentGeneration(1, 1), withDeploymentStatus(replica1, replica1, replica1)),
				csiSnapshotController: csiSnapshotController(withGenerations(1)),
			},
			expectedObjects: testObjects{
				crds:       getCRDs(withEstablishedConditions),
				deployment: getDeployment(argsLevel2, defaultImage, withDeploymentGeneration(1, 1), withDeploymentStatus(replica1, replica1, replica1)),
				csiSnapshotController: csiSnapshotController(
					withStatus(replica1),
					withGenerations(1),
					withTrueConditions(opv1.OperatorStatusTypeAvailable, opv1.OperatorStatusTypeUpgradeable, opv1.OperatorStatusTypePrereqsSatisfied),
					withFalseConditions(opv1.OperatorStatusTypeDegraded, opv1.OperatorStatusTypeProgressing)),
			},
		},
		{
			// Deployment has wrong nr. of replicas, modified by user, and gets replaced by the operator.
			name:  "deployment modified by user",
			image: defaultImage,
			initialObjects: testObjects{
				crds: getCRDs(withEstablishedConditions),
				deployment: getDeployment(argsLevel2, defaultImage,
					withDeploymentReplicas(2),      // User changed replicas
					withDeploymentGeneration(2, 1), // ... which changed Generation
					withDeploymentStatus(replica1, replica1, replica1)),
				csiSnapshotController: csiSnapshotController(withGenerations(1)), // the operator knows the old generation of the Deployment
			},
			expectedObjects: testObjects{
				crds: getCRDs(withEstablishedConditions),
				deployment: getDeployment(argsLevel2, defaultImage,
					withDeploymentReplicas(1),      // The operator fixed replica count
					withDeploymentGeneration(3, 1), // ... which bumps generation again
					withDeploymentStatus(replica1, replica1, replica1)),
				csiSnapshotController: csiSnapshotController(
					withStatus(replica1),
					withGenerations(3), // now the operator knows generation 1
					withTrueConditions(opv1.OperatorStatusTypeAvailable, opv1.OperatorStatusTypeUpgradeable, opv1.OperatorStatusTypePrereqsSatisfied, opv1.OperatorStatusTypeProgressing), // Progressing due to Generation change
					withFalseConditions(opv1.OperatorStatusTypeDegraded)),
			},
		},
		{
			// Deployment gets degraded from some reason
			name:  "deployment degraded",
			image: defaultImage,
			initialObjects: testObjects{
				crds: getCRDs(withEstablishedConditions),
				deployment: getDeployment(argsLevel2, defaultImage,
					withDeploymentGeneration(1, 1),
					withDeploymentStatus(0, 0, 0)), // the Deployment has no pods
				csiSnapshotController: csiSnapshotController(
					withStatus(replica1),
					withGenerations(1),
					withGeneration(1, 1),
					withTrueConditions(opv1.OperatorStatusTypeAvailable, opv1.OperatorStatusTypeUpgradeable, opv1.OperatorStatusTypePrereqsSatisfied),
					withFalseConditions(opv1.OperatorStatusTypeDegraded, opv1.OperatorStatusTypeProgressing)),
			},
			expectedObjects: testObjects{
				crds: getCRDs(withEstablishedConditions),
				deployment: getDeployment(argsLevel2, defaultImage,
					withDeploymentGeneration(1, 1),
					withDeploymentStatus(0, 0, 0)), // No change to the Deployment
				csiSnapshotController: csiSnapshotController(
					withStatus(replica0),
					withGenerations(1),
					withGeneration(1, 1),
					withTrueConditions(opv1.OperatorStatusTypeUpgradeable, opv1.OperatorStatusTypePrereqsSatisfied, opv1.OperatorStatusTypeProgressing), // The operator is Progressing
					withFalseConditions(opv1.OperatorStatusTypeDegraded, opv1.OperatorStatusTypeAvailable)),                                             // The operator is not Available (no replica is running...)
			},
		},
		{
			// Deployment is updating pods
			name:  "update",
			image: defaultImage,
			initialObjects: testObjects{
				crds: getCRDs(withEstablishedConditions),
				deployment: getDeployment(argsLevel2, defaultImage,
					withDeploymentGeneration(1, 1),
					withDeploymentStatus(1 /*ready*/, 1 /*available*/, 0 /*updated*/)), // the Deployment is updating 1 pod
				csiSnapshotController: csiSnapshotController(
					withStatus(replica1),
					withGenerations(1),
					withGeneration(1, 1),
					withTrueConditions(opv1.OperatorStatusTypeAvailable, opv1.OperatorStatusTypeUpgradeable, opv1.OperatorStatusTypePrereqsSatisfied),
					withFalseConditions(opv1.OperatorStatusTypeDegraded, opv1.OperatorStatusTypeProgressing)),
			},
			expectedObjects: testObjects{
				crds: getCRDs(withEstablishedConditions),
				deployment: getDeployment(argsLevel2, defaultImage,
					withDeploymentGeneration(1, 1),
					withDeploymentStatus(1, 1, 0)), // No change to the Deployment
				csiSnapshotController: csiSnapshotController(
					withStatus(replica0),
					withGenerations(1),
					withGeneration(1, 1),
					withTrueConditions(opv1.OperatorStatusTypeUpgradeable, opv1.OperatorStatusTypePrereqsSatisfied, opv1.OperatorStatusTypeAvailable, opv1.OperatorStatusTypeProgressing), // The operator is Progressing, but still Available
					withFalseConditions(opv1.OperatorStatusTypeDegraded)),
			},
		},
		{
			// User changes log level and it's projected into the Deployment
			name:  "log level change",
			image: defaultImage,
			initialObjects: testObjects{
				crds: getCRDs(withEstablishedConditions),
				deployment: getDeployment(argsLevel2, defaultImage,
					withDeploymentGeneration(1, 1),
					withDeploymentStatus(replica1, replica1, replica1)),
				csiSnapshotController: csiSnapshotController(
					withGenerations(1),
					withLogLevel(opv1.Trace), // User changed the log level...
					withGeneration(2, 1)),    //... which caused the Generation to increase
			},
			expectedObjects: testObjects{
				crds: getCRDs(withEstablishedConditions),
				deployment: getDeployment(argsLevel6, defaultImage, // The operator changed cmdline arguments with a new log level
					withDeploymentGeneration(2, 1), // ... which caused the Generation to increase
					withDeploymentStatus(replica1, replica1, replica1)),
				csiSnapshotController: csiSnapshotController(
					withStatus(replica1),
					withLogLevel(opv1.Trace),
					withGenerations(2),
					withGeneration(2, 2),
					withTrueConditions(opv1.OperatorStatusTypeAvailable, opv1.OperatorStatusTypeUpgradeable, opv1.OperatorStatusTypePrereqsSatisfied, opv1.OperatorStatusTypeProgressing), // Progressing due to Generation change
					withFalseConditions(opv1.OperatorStatusTypeDegraded)),
			},
		},
		// TODO: update of controller image
		{
			// error: timed out waiting for CRD to get Established condition
			name:  "timeout waiting for CRD",
			image: defaultImage,
			initialObjects: testObjects{
				csiSnapshotController: csiSnapshotController(),
			},
			expectedObjects: testObjects{
				crds:                  []*apiextv1.CustomResourceDefinition{getCRDs()[0]}, // Only the first CRD is created
				csiSnapshotController: csiSnapshotController(withTrueConditions(opv1.OperatorStatusTypeDegraded)),
			},
			expectErr: true,
		},
		{
			// error: v1alpha1 VolumeSnapshot already exists
			name:  "v1alpha1 VolumeSnapshot",
			image: defaultImage,
			initialObjects: testObjects{
				csiSnapshotController: csiSnapshotController(),
				crds:                  []*apiextv1.CustomResourceDefinition{getAlphaCRD("VolumeSnapshot")},
			},
			expectedObjects: testObjects{
				crds:                  []*apiextv1.CustomResourceDefinition{getAlphaCRD("VolumeSnapshot")},
				csiSnapshotController: csiSnapshotController(withTrueConditions(opv1.OperatorStatusTypeDegraded)),
			},
			expectErr: true,
			reactors: testReactors{
				crds: addCRDEstablishedRector,
			},
		},
		{
			// error: v1alpha1 VolumeSnapshotContent already exists
			name:  "v1alpha1 VolumeSnapshotContent",
			image: defaultImage,
			initialObjects: testObjects{
				csiSnapshotController: csiSnapshotController(),
				crds:                  []*apiextv1.CustomResourceDefinition{getAlphaCRD("VolumeSnapshotContent")},
			},
			expectedObjects: testObjects{
				crds:                  []*apiextv1.CustomResourceDefinition{getAlphaCRD("VolumeSnapshotContent")},
				csiSnapshotController: csiSnapshotController(withTrueConditions(opv1.OperatorStatusTypeDegraded)),
			},
			expectErr: true,
			reactors: testReactors{
				crds: addCRDEstablishedRector,
			},
		},
		{
			// error: v1alpha1 VolumeSnapshotClass already exists
			name:  "v1alpha1 VolumeSnapshotClass",
			image: defaultImage,
			initialObjects: testObjects{
				csiSnapshotController: csiSnapshotController(),
				crds:                  []*apiextv1.CustomResourceDefinition{getAlphaCRD("VolumeSnapshotClass")},
			},
			expectedObjects: testObjects{
				crds:                  []*apiextv1.CustomResourceDefinition{getAlphaCRD("VolumeSnapshotClass")},
				csiSnapshotController: csiSnapshotController(withTrueConditions(opv1.OperatorStatusTypeDegraded)),
			},
			expectErr: true,
			reactors: testReactors{
				crds: addCRDEstablishedRector,
			},
		},

		// TODO: more error cases? Deployment creation fails and things like that?
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Initialize
			ctx := newOperator(test)

			// Act
			err := ctx.operator.sync()

			// Assert
			// Check error
			if err != nil && !test.expectErr {
				t.Errorf("sync() returned unexpected error: %v", err)
			}
			if err == nil && test.expectErr {
				t.Error("sync() unexpectedly succeeded when error was expected")
			}

			// Check expectedObjects.crds
			actualCRDList, _ := ctx.extAPIClient.ApiextensionsV1().CustomResourceDefinitions().List(context.TODO(), metav1.ListOptions{})
			actualCRDs := map[string]*apiextv1.CustomResourceDefinition{}
			for i := range actualCRDList.Items {
				crd := &actualCRDList.Items[i]
				actualCRDs[crd.Name] = crd
			}
			expectedCRDs := map[string]*apiextv1.CustomResourceDefinition{}
			for _, crd := range test.expectedObjects.crds {
				expectedCRDs[crd.Name] = crd
			}

			for name, actualCRD := range actualCRDs {
				expectedCRD, found := expectedCRDs[name]
				if !found {
					t.Errorf("Unexpected CRD found: %s", name)
					continue
				}
				if !equality.Semantic.DeepEqual(expectedCRD, actualCRD) {
					t.Errorf("Unexpected CRD %+v content:\n%s", name, cmp.Diff(expectedCRD, actualCRD))
				}
				delete(expectedCRDs, name)
			}
			if len(expectedCRDs) > 0 {
				for _, crd := range expectedCRDs {
					t.Errorf("CRD %s not created by sync()", crd.Name)
				}
			}

			// Check expectedObjects.deployment
			if test.expectedObjects.deployment != nil {
				actualDeployment, err := ctx.coreClient.AppsV1().Deployments(targetNamespace).Get(context.TODO(), targetName, metav1.GetOptions{})
				if err != nil {
					t.Errorf("Failed to get Deployment %s: %v", targetName, err)
				}
				sanitizeDeployment(actualDeployment)
				sanitizeDeployment(test.expectedObjects.deployment)
				if !equality.Semantic.DeepEqual(test.expectedObjects.deployment, actualDeployment) {
					t.Errorf("Unexpected Deployment %+v content:\n%s", targetName, cmp.Diff(test.expectedObjects.deployment, actualDeployment))
				}
			}
			// Check expectedObjects.csiSnapshotController
			if test.expectedObjects.csiSnapshotController != nil {
				actualCSISnapshotController, err := ctx.operatorClient.OperatorV1().CSISnapshotControllers().Get(context.TODO(), globalConfigName, metav1.GetOptions{})
				if err != nil {
					t.Errorf("Failed to get CSISnapshotController %s: %v", globalConfigName, err)
				}
				sanitizeCSISnapshotController(actualCSISnapshotController)
				sanitizeCSISnapshotController(test.expectedObjects.csiSnapshotController)
				if !equality.Semantic.DeepEqual(test.expectedObjects.csiSnapshotController, actualCSISnapshotController) {
					t.Errorf("Unexpected CSISnapshotController %+v content:\n%s", targetName, cmp.Diff(test.expectedObjects.csiSnapshotController, actualCSISnapshotController))
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

	// Remove force annotations, they're random
	delete(deployment.Annotations, "operator.openshift.io/force")
	delete(deployment.Spec.Template.Annotations, "operator.openshift.io/force")
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
