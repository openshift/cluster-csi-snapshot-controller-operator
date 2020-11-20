module github.com/openshift/cluster-csi-snapshot-controller-operator

go 1.13

require (
	github.com/go-bindata/go-bindata v3.1.2+incompatible
	github.com/go-logr/logr v0.3.0 // indirect
	github.com/go-logr/zapr v0.2.0 // indirect
	github.com/google/go-cmp v0.5.0
	github.com/google/gofuzz v1.2.0 // indirect
	github.com/kubernetes-csi/external-snapshotter/client/v3 v3.0.0
	github.com/openshift/api v0.0.0-20201117184740-859beeffd973
	github.com/openshift/build-machinery-go v0.0.0-20200917070002-f171684f77ab
	github.com/openshift/client-go v0.0.0-20200827190008-3062137373b5
	github.com/openshift/library-go v0.0.0-20200918101923-1e4c94603efe
	github.com/prometheus/client_golang v1.7.1
	github.com/spf13/cobra v1.0.0
	github.com/spf13/pflag v1.0.5
	go.uber.org/multierr v1.6.0 // indirect
	go.uber.org/zap v1.16.0 // indirect
	golang.org/x/net v0.0.0-20201110031124-69a78807bb2b // indirect
	golang.org/x/text v0.3.4 // indirect
	google.golang.org/protobuf v1.25.0 // indirect
	k8s.io/api v0.19.4
	k8s.io/apiextensions-apiserver v0.19.0
	k8s.io/apimachinery v0.19.4
	k8s.io/client-go v0.19.0
	k8s.io/component-base v0.19.0
	k8s.io/klog/v2 v2.4.0
	sigs.k8s.io/controller-runtime v0.6.3
	sigs.k8s.io/structured-merge-diff/v4 v4.0.2 // indirect
)
