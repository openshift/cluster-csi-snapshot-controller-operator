module github.com/openshift/cluster-csi-snapshot-controller-operator

go 1.13

require (
	github.com/golang/glog v0.0.0-20160126235308-23def4e6c14b
	github.com/jteeuwen/go-bindata v3.0.8-0.20151023091102-a0ff2567cfb7+incompatible
	github.com/openshift/api v0.0.0-20200116145750-0e2ff1e215dd
	github.com/openshift/client-go v0.0.0-20200116152001-92a2713fa240
	github.com/openshift/library-go v0.0.0-20200110123007-276971bef92b
	github.com/prometheus/client_golang v1.1.0
	github.com/spf13/cobra v0.0.5
	github.com/spf13/pflag v1.0.5
	k8s.io/apiextensions-apiserver v0.17.0
	k8s.io/apimachinery v0.17.1
	k8s.io/client-go v0.17.1
	k8s.io/component-base v0.17.0
	monis.app/go v0.0.0-20190702030534-c65526068664
	sigs.k8s.io/controller-runtime v0.4.0
)
