package framework

import (
	"os"

	volumesnapshotsv1beta1 "github.com/kubernetes-csi/external-snapshotter/client/v3/clientset/versioned/typed/volumesnapshot/v1beta1"
	clientapiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1"
	appsv1client "k8s.io/client-go/kubernetes/typed/apps/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
)

// ClientSet contains the interfaces necessary for the CSI Snapshot Controller Operator to
// interact with appropriate resources (PV,PVC, VolumeSnapshot, etc.) during testing.
type ClientSet struct {
	corev1client.CoreV1Interface
	appsv1client.AppsV1Interface
	clientapiextensionsv1.ApiextensionsV1Interface
	volumesnapshotsv1beta1.SnapshotV1beta1Interface
}

// NewClientSet returns a client to interface with the OCP cluster.
func NewClientSet(kubeconfig string) *ClientSet {
	var config *rest.Config
	var err error

	if kubeconfig == "" {
		kubeconfig = os.Getenv("KUBECONFIG")
	}

	if kubeconfig != "" {
		klog.V(4).Infof("Loading kube client config from path %q", kubeconfig)
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		klog.V(4).Infof("Using in-cluster kube client config")
		config, err = rest.InClusterConfig()
	}
	if err != nil {
		panic(err)
	}

	clientSet := &ClientSet{}
	clientSet.CoreV1Interface = corev1client.NewForConfigOrDie(config)
	clientSet.ApiextensionsV1Interface = clientapiextensionsv1.NewForConfigOrDie(config)
	clientSet.AppsV1Interface = appsv1client.NewForConfigOrDie(config)
	clientSet.SnapshotV1beta1Interface = volumesnapshotsv1beta1.NewForConfigOrDie(config)

	return clientSet
}
