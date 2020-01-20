package common

import (
	"os"

	apiext "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog"
)

// Builder can create a variety of kubernetes client interface
// with its embedded rest.Config.
type Builder struct {
	config *rest.Config
}

// APIExtClientOrDie returns the kubernetes client interface for extended kubernetes objects.
func (cb *Builder) APIExtClientOrDie(name string) apiext.Interface {
	return apiext.NewForConfigOrDie(rest.AddUserAgent(cb.config, name))
}

// KubeClientOrDie returns the kubernetes client interface for general kubernetes objects.
func (cb *Builder) KubeClientOrDie(name string) kubernetes.Interface {
	return kubernetes.NewForConfigOrDie(rest.AddUserAgent(cb.config, name))
}

// NewBuilder returns a *ClientBuilder with the given kubeconfig.
func NewBuilder(kubeconfig string) (*Builder, error) {
	var config *rest.Config
	var err error

	if kubeconfig == "" {
		kubeconfig = os.Getenv("KUBECONFIG")
	}

	if kubeconfig != "" {
		klog.Infof("Loading kube client config from path %q", kubeconfig)
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		klog.Infof("Using in-cluster kube client config")
		config, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, err
	}

	return &Builder{
		config: config,
	}, nil
}
