module github.com/openshift/cluster-csi-snapshot-controller-operator

go 1.16

require (
	github.com/go-bindata/go-bindata v3.1.2+incompatible
	github.com/go-logr/zapr v0.2.0 // indirect
	github.com/google/go-cmp v0.5.5
	github.com/google/gofuzz v1.2.0 // indirect
	github.com/kubernetes-csi/external-snapshotter/client/v4 v4.2.0
	github.com/openshift/api v0.0.0-20210730095913-85e1d547cdee
	github.com/openshift/build-machinery-go v0.0.0-20210712174854-1bb7fd1518d3
	github.com/openshift/client-go v0.0.0-20210730113412-1811c1b3fc0e
	github.com/openshift/library-go v0.0.0-20210817120645-b59564e63303
	github.com/prometheus/client_golang v1.11.0
	github.com/spf13/cobra v1.1.3
	github.com/spf13/pflag v1.0.5
	k8s.io/api v0.22.0-rc.0
	k8s.io/apiextensions-apiserver v0.22.0-rc.0
	k8s.io/apimachinery v0.22.0-rc.0
	k8s.io/client-go v0.22.0-rc.0
	k8s.io/component-base v0.22.0-rc.0
	k8s.io/klog/v2 v2.9.0
	sigs.k8s.io/controller-runtime v0.6.3
)
