package e2e

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	framework "github.com/openshift/cluster-csi-snapshot-controller-operator/test/framework"
	kapierrs "k8s.io/apimachinery/pkg/api/errors"

	volumesnapshotsv1beta1 "github.com/kubernetes-csi/external-snapshotter/client/v3/apis/volumesnapshot/v1beta1"
)

const (
	snapshotGroup      = "snapshot.storage.k8s.io"
	snapshotAPIVersion = "snapshot.storage.k8s.io/v1beta1"

	operatorNamespace = "openshift-cluster-storage-operator"
	operatorName      = "csi-snapshot-controller-operator"
)

var (
	retryInterval        = time.Second * 5
	timeout              = time.Second * 120
	cleanupRetryInterval = time.Second * 1
	cleanupTimeout       = time.Second * 5
	poll                 = 2 * time.Second
	size                 = 1000000
)

func TestCSISnapshotControllerOperator(t *testing.T) {
	client := framework.NewClientSet("")
	driverName := GenerateDriverName()

	// Create a namespace for the subsequent objects
	namespace, err := createNamespace(t, client)
	if err != nil {
		t.Fatalf("Unable to create a corresponding namespace: %v", err)
	}
	defer func() {
		err = client.CoreV1Interface.Namespaces().Delete(context.TODO(), namespace, metav1.DeleteOptions{})
		if err != nil {
			t.Errorf("Error attempting to delete the namespace: %v", err)
		}
	}()

	// Ensure that the Operator is ready
	err = waitForOperatorToBeReady(t, client)
	if err != nil {
		t.Fatalf("ClusterOperator never became ready: %v", err)
	}

	// Ensure the VolumeSnapshot CRD is installed
	_, err = client.ApiextensionsV1Interface.CustomResourceDefinitions().Get(context.TODO(), "volumesnapshots.snapshot.storage.k8s.io", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Error attempting to retrieve snapshot CRD: %v", err)
	}

	// Create the PV
	pv := CreatePV(driverName, "pv", size)
	pv, err = client.CoreV1Interface.PersistentVolumes().Create(context.TODO(), pv, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Error attempting to create PV: %v", err)
	}
	defer func() {
		err = client.CoreV1Interface.PersistentVolumes().Delete(context.TODO(), pv.ObjectMeta.Name, metav1.DeleteOptions{})
		if err != nil {
			t.Errorf("Error attempting to delete PV: %v", err)
		}
	}()

	// Create the PVC
	pvc := CreatePVC(namespace, pv.Spec.StorageClassName, pv.ObjectMeta.UID, size)
	pvc, err = client.CoreV1Interface.PersistentVolumeClaims(namespace).Create(context.TODO(), pvc, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Error attempting to create PVC: %v", err)
	}
	defer func() {
		err = client.CoreV1Interface.PersistentVolumeClaims(namespace).Delete(context.TODO(), pvc.ObjectMeta.Name, metav1.DeleteOptions{})
		if err != nil {
			t.Errorf("Error attempting to delete PVC: %v", err)
		}
	}()

	// Create the VolumeSnapshotClass
	snapshotClass := CreateFakeSnapshotClass(driverName)
	snapshotClass, err = client.SnapshotV1beta1Interface.VolumeSnapshotClasses().Create(context.TODO(), snapshotClass, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Error attempting to create VolumeSnapshotClass: %v", err)
	}
	defer func() {
		err = client.SnapshotV1beta1Interface.VolumeSnapshotClasses().Delete(context.TODO(), snapshotClass.ObjectMeta.Name, metav1.DeleteOptions{})
		if err != nil {
			t.Errorf("Error attempting to delete VolumeSnapshotClass: %v", err)
		}
	}()
	t.Logf("Created snapshotClass: %v", snapshotClass.ObjectMeta.Name)

	// Create the VolumeSnapshot
	snapshot := CreateFakeSnapshot(pvc.ObjectMeta.Name, namespace, snapshotClass.ObjectMeta.Name, pvc.ObjectMeta.UID)
	snapshot, err = client.SnapshotV1beta1Interface.VolumeSnapshots(namespace).Create(context.TODO(), snapshot, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Error attempting to create VolumeSnapshot: %v", err)
	}
	defer func() {
		err = client.SnapshotV1beta1Interface.VolumeSnapshots(namespace).Delete(context.TODO(), snapshot.ObjectMeta.Name, metav1.DeleteOptions{})
		if err != nil {
			t.Errorf("Error attempting to delete VolumeSnapshot: %v", err)
		}
	}()
	t.Logf("Created snapshot %v", snapshot.ObjectMeta.Name)

	// Search for a matching VolumeSnapshotContent
	snapshotContent, err := waitForSnapshotContent(t, client, snapshot)
	if err != nil {
		t.Fatalf("Unable to find a matching VolumeSnapshotContent within the expected time: %v", err)
	}
	// Clean up the VolumeSnapshotContent
	defer func() {
		err = deleteSnapshotContent(t, client, snapshotContent)
		if err != nil {
			t.Logf("Error: %v", err)
		}
	}()
	t.Logf("Found snapshot content %v", snapshotContent.ObjectMeta.Name)

	err = markSnapshotContentReady(t, client, snapshotContent)
	if err != nil {
		t.Fatalf("Unable to update VolumeSnapshotContent's status: %v", err)
	}

	// Ensure the Snapshot reports ReadyToUse = true
	err = waitForSnapshotReady(client, snapshot, t, namespace)
	if err != nil {
		t.Fatalf("Snapshot not ready: %v", err)
	}

}

// createNamespace generates a string that uses the current time as a seed,
// and then creates a corresponding namespace in the cluster.
func createNamespace(t *testing.T, client *framework.ClientSet) (string, error) {
	name := "test-" + strconv.FormatInt(time.Now().Unix(), 10)
	t.Logf("Creating namespace: %s", name)
	namespaceObj := &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	_, err := client.CoreV1Interface.Namespaces().Create(context.TODO(), namespaceObj, metav1.CreateOptions{})
	if err != nil {
		return "", err
	}
	return name, nil
}

// waitForSnapshotContent continually looks for a VolumeSnapshotContent that matches
// the passed in VolumeSnapshot. It will search until a VolumeSnapshotContent is found
// or until the timeout occurs.
func waitForSnapshotContent(t *testing.T, client *framework.ClientSet, snapshot *volumesnapshotsv1beta1.VolumeSnapshot) (*volumesnapshotsv1beta1.VolumeSnapshotContent, error) {
	snapshotName := snapshot.ObjectMeta.Name
	t.Logf("Waiting up to %v for VolumeSnapshotContent to be found that matches %s", timeout, snapshotName)
	for start := time.Now(); time.Since(start) < timeout; time.Sleep(poll) {
		content, err := client.SnapshotV1beta1Interface.VolumeSnapshotContents().List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			t.Fatalf("Unable to retrieve list of VolumeSnapshotContents: %v", err)
			continue
		} else {
			for _, v := range content.Items {
				if v.Spec.VolumeSnapshotRef.Name == snapshotName {
					t.Logf("Found matching VolumeSnapshotContent within %v", time.Since(start))
					return &v, nil
				}
			}
			t.Logf("VolumeSnapshotContents found, but none match %s", snapshotName)
		}
	}
	return nil, fmt.Errorf("Unable to find matching VolumeSnapshotContent within %v", timeout)
}

// deleteSnapshotContent was created due to issues with Finalizers being added
// to the VolumeSnapshotContent. It will attempt to delete the VolumeSnapshotContent,
// remove all Finalizers, and then query until the VolumeSnapshotContent can no longer
// be found.
func deleteSnapshotContent(t *testing.T, client *framework.ClientSet, snapshotContent *volumesnapshotsv1beta1.VolumeSnapshotContent) error {
	name := snapshotContent.ObjectMeta.Name
	deleted := false
	t.Logf("Waiting up to 2m0s for snapshotContent %s to be deleted", name)
	for start := time.Now(); time.Since(start) < timeout; time.Sleep(poll) {
		clusterContent, err := client.SnapshotV1beta1Interface.VolumeSnapshotContents().Get(context.TODO(), name, metav1.GetOptions{})
		if err != nil && !kapierrs.IsNotFound(err) {
			t.Fatalf("Error attempting to get VolumeSnapshotContent: %v", err)
			continue
		} else if kapierrs.IsNotFound(err) {
			// Can't find the content, indicating it's been successfully deleted
			t.Logf("VolumeSnapshotContent %s successfully deleted", name)
			return nil
		} else if !deleted {
			err := client.SnapshotV1beta1Interface.VolumeSnapshotContents().Delete(context.TODO(), name, metav1.DeleteOptions{})
			if err != nil {
				return err
			}
			deleted = true
		} else {
			clusterContent.ObjectMeta.Finalizers = nil
			_, err = client.SnapshotV1beta1Interface.VolumeSnapshotContents().Update(context.TODO(), clusterContent, metav1.UpdateOptions{})
			if err != nil {
				return err
			}
		}
	}
	return fmt.Errorf("Error deleting snapshot %s in appropriate time", name)
}

// markSnapshotContentReady updates the status of the passed in VolumeSnapshotContent
// to mark ReadyToUse true.
func markSnapshotContentReady(t *testing.T, client *framework.ClientSet, snapshotContent *volumesnapshotsv1beta1.VolumeSnapshotContent) error {
	ready := true
	status := volumesnapshotsv1beta1.VolumeSnapshotContentStatus{
		ReadyToUse: &ready,
	}
	snapshotContent.Status = &status
	for start := time.Now(); time.Since(start) < timeout; time.Sleep(poll) {
		_, err := client.SnapshotV1beta1Interface.VolumeSnapshotContents().UpdateStatus(context.TODO(), snapshotContent, metav1.UpdateOptions{})
		if err != nil {
			t.Logf("Error updating VolumeSnapshotContent's %v status, retrying in %v: %v", snapshotContent.ObjectMeta.Name, poll, err)
			continue
		} else {
			return nil
		}
	}
	return fmt.Errorf("VolumeSnapshotContent status was unable to be updated within %v", timeout)
}

// WaitForSnapshotReady waits for a VolumeSnapshot to be ready to use
// or until the timeout occurs, whichever comes first.
func waitForSnapshotReady(client *framework.ClientSet, snapshot *volumesnapshotsv1beta1.VolumeSnapshot, t *testing.T, namespace string) error {
	snapshotName := snapshot.ObjectMeta.Name
	t.Logf("Waiting up to %v for VolumeSnapshot %s to become ready", timeout, snapshotName)
	for start := time.Now(); time.Since(start) < timeout; time.Sleep(poll) {
		snapshot, err := client.SnapshotV1beta1Interface.VolumeSnapshots(namespace).Get(context.TODO(), snapshot.ObjectMeta.Name, metav1.GetOptions{})
		if err != nil {
			t.Logf("Failed to get claim %q, retrying in %v. Error: %v", snapshotName, poll, err)
			continue
		} else if snapshot == nil {
			t.Logf("VolumeSnapshot %s not created within %v", snapshotName, time.Since(start))
		} else {
			status := *snapshot.Status.ReadyToUse
			if &status != nil && status {
				t.Logf("VolumeSnapshot %s found and is ready within %v", snapshotName, time.Since(start))
				return nil
			}
			t.Logf("VolumeSnapshot %s found but is not ready.", snapshotName)
		}
	}
	return fmt.Errorf("VolumeSnapshot %s is not ready within %v", snapshotName, timeout)
}

// Runs in a loop waiting until there is at least 1 available replica
// in the CSI Snapshot Controller Operator or until the timeout occurs
func waitForOperatorToBeReady(t *testing.T, client *framework.ClientSet) error {
	t.Log("Waiting for csi-snapshot-controller to be ready...")
	for start := time.Now(); time.Since(start) < timeout; time.Sleep(poll) {
		deployment, err := client.AppsV1Interface.Deployments(operatorNamespace).Get(context.TODO(), operatorName, metav1.GetOptions{})
		if err != nil {
			t.Logf("Failed to get ClusterOperator %s, retrying in %v. Error: %v", operatorName, poll, err)
			continue
		} else {
			available := deployment.Status.AvailableReplicas
			if available >= 1 {
				t.Logf("ClusterOperator %s found and is ready within %v", operatorName, time.Since(start))
				return nil
			}
			t.Logf("ClusterOperator %s found but is not ready", operatorName)
		}
	}
	return fmt.Errorf("ClusterOperator %s is not ready within %v", operatorName, timeout)
}
