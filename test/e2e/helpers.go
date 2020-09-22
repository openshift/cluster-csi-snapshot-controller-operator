package e2e

import (
	"math/rand"
	"strconv"
	"time"

	volumesnapshotsv1beta1 "github.com/kubernetes-csi/external-snapshotter/client/v3/apis/volumesnapshot/v1beta1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// CreatePV returns a fake PersistentVolume with the provided parameters
func CreatePV(driver, volume string, size int) *corev1.PersistentVolume {
	name := "pv" + strconv.Itoa(rand.Intn(1000))
	pv := &corev1.PersistentVolume{
		TypeMeta: metav1.TypeMeta{
			Kind:       "PersistentVolume",
			APIVersion: corev1.SchemeGroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: corev1.PersistentVolumeSpec{
			StorageClassName: driver,
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver:       driver,
					VolumeHandle: volume,
				},
			},
			Capacity: corev1.ResourceList{
				"storage": *resource.NewQuantity(int64(size), resource.BinarySI),
			},
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		},
	}

	return pv
}

// CreatePVC returns a fake PersistentVolumeClaim with the provided parameters
func CreatePVC(namespace, scName string, uid types.UID, size int) *corev1.PersistentVolumeClaim {
	pvcName := "pvc" + strconv.Itoa(rand.Intn(1000))
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:       pvcName,
			Namespace:  namespace,
			UID:        uid,
			Finalizers: []string{},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					"storage": *resource.NewQuantity(int64(size), resource.BinarySI),
				},
			},
		},
	}
	if scName != "" {
		pvc.Spec.StorageClassName = &scName
	}
	return pvc
}

// CreateFakeSnapshot returns a fake VolumeSnapshot with the provided parameters
func CreateFakeSnapshot(claimName, namespace, snapshotClassName string, uid types.UID) *volumesnapshotsv1beta1.VolumeSnapshot {
	snapshotName := "test-snapshot" + strconv.Itoa(rand.Intn(1000))
	time := metav1.NewTime(time.Now())
	snapshot := &volumesnapshotsv1beta1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:       snapshotName,
			Namespace:  namespace,
			UID:        uid,
			Finalizers: []string{},
		},
		Spec: volumesnapshotsv1beta1.VolumeSnapshotSpec{
			VolumeSnapshotClassName: &snapshotClassName,
			Source: volumesnapshotsv1beta1.VolumeSnapshotSource{
				PersistentVolumeClaimName: &claimName,
			},
		},
		Status: &volumesnapshotsv1beta1.VolumeSnapshotStatus{
			CreationTime: &time,
		},
	}
	return snapshot
}

// CreateFakeSnapshotClass returns a fake VolumeSnapshotClass with the provided parameters
func CreateFakeSnapshotClass(driver string) *volumesnapshotsv1beta1.VolumeSnapshotClass {
	name := "test-snapshotclass" + strconv.Itoa(rand.Intn(1000))
	class := &volumesnapshotsv1beta1.VolumeSnapshotClass{
		ObjectMeta:     metav1.ObjectMeta{Name: name},
		Driver:         driver,
		DeletionPolicy: volumesnapshotsv1beta1.VolumeSnapshotContentRetain,
	}
	return class
}

// GenerateDriverName creates a random name to use for the driver
func GenerateDriverName() string {
	return "test-driver-" + strconv.Itoa(rand.Intn(1000))
}
