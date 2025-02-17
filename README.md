# csi-snapshot-controller-operator

The CSI snapshot controller operator is an
[OpenShift ClusterOperator](https://github.com/openshift/enhancements/blob/master/dev-guide/operators.md#what-is-an-openshift-clusteroperator).
It installs and maintains the CSI Snapshot Controller, which is responsible for watching the VolumeSnapshot CRD objects and manages the creation and deletion lifecycle of volume snapshots.
