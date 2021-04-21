module github.com/openshift/cluster-csi-snapshot-controller-operator

go 1.16

require (
	github.com/go-bindata/go-bindata v3.1.2+incompatible
	github.com/go-logr/zapr v0.2.0 // indirect
	github.com/google/go-cmp v0.5.5
	github.com/google/gofuzz v1.2.0 // indirect
	github.com/kubernetes-csi/external-snapshotter/client/v4 v4.0.0
	github.com/openshift/api v0.0.0-20210415092137-8c78458f83d9
	github.com/openshift/build-machinery-go v0.0.0-20210209125900-0da259a2c359
	github.com/openshift/client-go v0.0.0-20210331195552-cf6c2669e01f
	github.com/openshift/library-go v0.0.0-20210414082648-6e767630a0dc
	github.com/prometheus/client_golang v1.7.1
	github.com/spf13/cobra v1.1.1
	github.com/spf13/pflag v1.0.5
	go.uber.org/multierr v1.6.0 // indirect
	go.uber.org/zap v1.16.0 // indirect
	golang.org/x/net v0.0.0-20210414194228-064579744ee0 // indirect
	k8s.io/api v0.21.0
	k8s.io/apiextensions-apiserver v0.21.0
	k8s.io/apimachinery v0.21.0
	k8s.io/client-go v0.21.0
	k8s.io/component-base v0.21.0
	k8s.io/klog/v2 v2.8.0
	sigs.k8s.io/controller-runtime v0.6.3
	sigs.k8s.io/structured-merge-diff/v4 v4.1.1 // indirect
)
