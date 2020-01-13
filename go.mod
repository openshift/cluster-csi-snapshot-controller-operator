module github.com/openshift/cluster-csi-snapshot-controller-operator

go 1.13

require (
	github.com/openshift/api v0.0.0-20191217141120-791af96035a5
	github.com/openshift/client-go v0.0.0-20191216194936-57f413491e9e
	github.com/openshift/library-go v0.0.0-20200110123007-276971bef92b
	github.com/prometheus/client_golang v1.1.0
	github.com/spf13/cobra v0.0.5
	github.com/spf13/pflag v1.0.5
	k8s.io/apiextensions-apiserver v0.17.0
	k8s.io/apimachinery v0.17.0
	k8s.io/client-go v0.17.0
	k8s.io/component-base v0.17.0
	monis.app/go v0.0.0-20190702030534-c65526068664
	sigs.k8s.io/controller-runtime v0.4.0
)
