package backup

import (
	"context"
	"fmt"

	v2 "github.com/infinispan/infinispan-operator/pkg/apis/infinispan/v2alpha1"
	"github.com/infinispan/infinispan-operator/pkg/controller/constants"
	ispnctrl "github.com/infinispan/infinispan-operator/pkg/controller/infinispan"
	zero "github.com/infinispan/infinispan-operator/pkg/controller/zerocapacity"
	"github.com/infinispan/infinispan-operator/pkg/infinispan/backup"
	"github.com/infinispan/infinispan-operator/pkg/infinispan/client/http"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

const (
	ControllerName = "backup-controller"
	DataMountPath  = "/opt/infinispan/backups"
)

var ctx = context.Background()

// reconcileBackup reconciles a Backup object
type reconcileBackup struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client.Client
}

type backupResource struct {
	instance *v2.Backup
	client   client.Client
	scheme   *runtime.Scheme
}

func Add(mgr manager.Manager) error {
	return zero.CreateController(ControllerName, &reconcileBackup{mgr.GetClient()}, mgr)
}

func (r *reconcileBackup) ResourceInstance(key types.NamespacedName, ctrl *zero.Controller) (zero.Resource, error) {
	instance := &v2.Backup{}
	if err := ctrl.Get(ctx, key, instance); err != nil {
		return nil, err
	}

	instance.Spec.ApplyDefaults()
	return &backupResource{
		instance: instance,
		client:   r.Client,
		scheme:   ctrl.Scheme,
	}, nil
}

func (r *reconcileBackup) Type() runtime.Object {
	return &v2.Backup{}
}

func (r *backupResource) AsMeta() metav1.Object {
	return r.instance
}

func (r *backupResource) Cluster() string {
	return r.instance.Spec.Cluster
}

func (r *backupResource) Phase() zero.Phase {
	return zero.Phase(r.instance.Status.Phase)
}

func (r *backupResource) UpdatePhase(phase zero.Phase, phaseErr error) error {
	instance := r.instance
	if err := r.client.Get(ctx, types.NamespacedName{Namespace: instance.Namespace, Name: instance.Name}, instance); err != nil {
		return fmt.Errorf("Failed to reload Backup status: %w", err)
	}

	backupPhase := v2.BackupPhase(phase)
	var reason string
	if phaseErr != nil {
		reason = phaseErr.Error()
	}

	if instance.Status.Phase == backupPhase && instance.Status.Reason == reason {
		// Phase already updated so do nothing
		return nil
	}
	instance.Status.Phase = backupPhase
	instance.Status.Reason = reason

	if err := r.client.Status().Update(ctx, r.instance); err != nil {
		return fmt.Errorf("Failed to update Backup status: %w", err)
	}
	return nil
}

func (r *backupResource) Init() (*zero.Spec, error) {
	err := r.getOrCreatePvc()
	if err != nil {
		return nil, err
	}

	// Status is updated in the zero_controller when UpdatePhase is called
	r.instance.Status.PVC = fmt.Sprintf("pvc/%s", r.instance.Name)
	return &zero.Spec{
		Volume: zero.VolumeSpec{
			UpdatePermissions: true,
			MountPath:         DataMountPath,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: r.instance.Name,
				},
			},
		},
		Container: r.instance.Spec.Container,
		PodLabels: PodLabels(r.instance.Name, r.instance.Spec.Cluster),
	}, nil
}

func (r *backupResource) getOrCreatePvc() error {
	pvc := &corev1.PersistentVolumeClaim{}
	err := r.client.Get(ctx, types.NamespacedName{
		Name:      r.instance.Name,
		Namespace: r.instance.Namespace,
	}, pvc)

	// If the pvc already exists simply return
	if err == nil {
		return nil
	}

	if !errors.IsNotFound(err) {
		return err
	}

	volumeSpec := r.instance.Spec.Volume
	var storage resource.Quantity
	if volumeSpec.Storage == nil {
		// TODO calculate based upon number of Pods in cluster
		// ISPN- Utilise backup size estimate
		storage = constants.DefaultPVSize
	} else {
		storage, err = resource.ParseQuantity(*volumeSpec.Storage)
		if err != nil {
			return err
		}
	}

	// TODO add labels
	pvc = &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      r.instance.Name,
			Namespace: r.instance.Namespace,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{
				corev1.ReadWriteOnce,
			},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: storage,
				},
			},
			StorageClassName: volumeSpec.StorageClassName,
		},
	}
	controllerutil.SetControllerReference(r.instance, pvc, r.scheme)
	if err = r.client.Create(ctx, pvc); err != nil {
		return fmt.Errorf("Unable to create pvc: %w", err)
	}
	return nil
}

func (r *backupResource) Exec(client http.HttpClient) error {
	instance := r.instance
	backupManager := backup.NewManager(instance.Name, client)
	var resources backup.Resources
	if instance.Spec.Resources == nil {
		resources = backup.Resources{}
	} else {
		resources = backup.Resources(*instance.Spec.Resources)
	}
	config := &backup.BackupConfig{
		Directory: DataMountPath,
		Resources: resources,
	}
	return backupManager.Backup(instance.Name, config)
}

func (r *backupResource) ExecStatus(client http.HttpClient) (zero.Phase, error) {
	name := r.instance.Name
	backupManager := backup.NewManager(name, client)

	status, err := backupManager.BackupStatus(name)
	return zero.Phase(status), err
}

func PodLabels(backup, cluster string) map[string]string {
	m := ispnctrl.ServiceLabels(cluster)
	m["backup_cr"] = backup
	return m
}
