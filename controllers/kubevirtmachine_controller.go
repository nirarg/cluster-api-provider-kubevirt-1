/*
Copyright 2021 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	gocontext "context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/cluster-api-provider-kubevirt/pkg/ssh"

	infrav1 "sigs.k8s.io/cluster-api-provider-kubevirt/api/v1alpha4"
	"sigs.k8s.io/cluster-api-provider-kubevirt/pkg/context"
	kubevirthandler "sigs.k8s.io/cluster-api-provider-kubevirt/pkg/kubevirt"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1alpha4"
	"sigs.k8s.io/cluster-api/util"
	clusterutil "sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/cluster-api/util/predicates"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// KubevirtMachineReconciler reconciles a KubevirtMachine object.
type KubevirtMachineReconciler struct {
	client.Client
}

// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=kubevirtmachines,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=kubevirtmachines/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters;machines,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets;,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kubevirt.io,resources=virtualmachines;,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kubevirt.io,resources=virtualmachineinstances;,verbs=get;list;watch

// Reconcile handles KubevirtMachine events.
func (r *KubevirtMachineReconciler) Reconcile(goctx gocontext.Context, req ctrl.Request) (_ ctrl.Result, rerr error) {
	log := ctrl.LoggerFrom(goctx)

	// Fetch the KubevirtMachine instance.
	kubevirtMachine := &infrav1.KubevirtMachine{}
	if err := r.Client.Get(goctx, req.NamespacedName, kubevirtMachine); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Fetch the Machine.
	machine, err := util.GetOwnerMachine(goctx, r.Client, kubevirtMachine.ObjectMeta)
	if err != nil {
		return ctrl.Result{}, err
	}
	if machine == nil {
		log.Info("Waiting for Machine Controller to set OwnerRef on KubevirtMachine")
		return ctrl.Result{}, nil
	}

	log = log.WithValues("machine", machine.Name)

	// Fetch the Cluster.
	cluster, err := util.GetClusterFromMetadata(goctx, r.Client, machine.ObjectMeta)
	if err != nil {
		log.Info("KubevirtMachine owner Machine is missing cluster label or cluster does not exist")
		return ctrl.Result{}, err
	}
	if cluster == nil {
		log.Info(fmt.Sprintf("Please associate this machine with a cluster using the label %s: <name of cluster>", clusterv1.ClusterLabelName))
		return ctrl.Result{}, nil
	}

	log = log.WithValues("cluster", cluster.Name)

	// Fetch the KubevirtCluster.
	kubevirtCluster := &infrav1.KubevirtCluster{}
	kubevirtClusterName := client.ObjectKey{
		Namespace: kubevirtMachine.Namespace,
		Name:      cluster.Spec.InfrastructureRef.Name,
	}
	if err := r.Client.Get(goctx, kubevirtClusterName, kubevirtCluster); err != nil {
		log.Info("KubevirtCluster is not available yet")
		return ctrl.Result{}, nil
	}

	log = log.WithValues("kubevirt-cluster", kubevirtCluster.Name)

	// Create the machine context for this request.
	machineContext := &context.MachineContext{
		Context:         goctx,
		Cluster:         cluster,
		KubevirtCluster: kubevirtCluster,
		Machine:         machine,
		KubevirtMachine: kubevirtMachine,
		Logger:          ctrl.LoggerFrom(goctx).WithName(req.Namespace).WithName(req.Name),
	}

	// Initialize the patch helper
	patchHelper, err := patch.NewHelper(kubevirtMachine, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Always attempt to Patch the KubevirtMachine object and status after each reconciliation.
	defer func() {
		if err := machineContext.PatchKubevirtMachine(patchHelper); err != nil {
			machineContext.Logger.Error(err, "failed to patch KubevirtMachine")
			if rerr == nil {
				rerr = err
			}
		}
	}()

	// Add finalizer first if not exist to avoid the race condition between init and delete
	if !controllerutil.ContainsFinalizer(kubevirtMachine, infrav1.MachineFinalizer) {
		controllerutil.AddFinalizer(kubevirtMachine, infrav1.MachineFinalizer)
		return ctrl.Result{}, nil
	}

	// Check if the infrastructure is ready, otherwise return and wait for the cluster object to be updated
	if !cluster.Status.InfrastructureReady {
		log.Info("Waiting for KubevirtCluster Controller to create cluster infrastructure")
		conditions.MarkFalse(kubevirtMachine, infrav1.VMProvisionedCondition, infrav1.WaitingForClusterInfrastructureReason, clusterv1.ConditionSeverityInfo, "")
		return ctrl.Result{}, nil
	}

	// Handle deleted machines
	if !kubevirtMachine.ObjectMeta.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(machineContext)
	}

	// Handle non-deleted machines
	return r.reconcileNormal(machineContext)
}

func (r *KubevirtMachineReconciler) reconcileNormal(ctx *context.MachineContext) (res ctrl.Result, retErr error) {
	// If the machine is already provisioned, return
	if ctx.KubevirtMachine.Spec.ProviderID != nil {
		// ensure ready state is set.
		// This is required after move, because status is not moved to the target cluster.
		ctx.KubevirtMachine.Status.Ready = true
		conditions.MarkTrue(ctx.KubevirtMachine, infrav1.VMProvisionedCondition)
		return ctrl.Result{}, nil
	}

	// Make sure bootstrap data is available and populated.
	if ctx.Machine.Spec.Bootstrap.DataSecretName == nil {
		if !util.IsControlPlaneMachine(ctx.Machine) && !conditions.IsTrue(ctx.Cluster, clusterv1.ControlPlaneInitializedCondition) {
			ctx.Logger.Info("Waiting for the control plane to be initialized")
			conditions.MarkFalse(ctx.KubevirtMachine, infrav1.VMProvisionedCondition, clusterv1.WaitingForControlPlaneAvailableReason, clusterv1.ConditionSeverityInfo, "")
			return ctrl.Result{}, nil
		}

		ctx.Logger.Info("Waiting for the Bootstrap provider controller to set bootstrap data")
		conditions.MarkFalse(ctx.KubevirtMachine, infrav1.VMProvisionedCondition, infrav1.WaitingForBootstrapDataReason, clusterv1.ConditionSeverityInfo, "")
		return ctrl.Result{}, nil
	}

	clusterContext := &context.ClusterContext{
		Context:         ctx.Context,
		Cluster:         ctx.Cluster,
		KubevirtCluster: ctx.KubevirtCluster,
		Logger:          ctx.Logger,
	}

	// Fetch SSH keys to be used for cluster nodes, and update bootstrap script cloud-init with public key
	clusterNodeSshKeys := ssh.NewClusterNodeSshKeys(clusterContext, r.Client)
	if persisted := clusterNodeSshKeys.IsPersistedToSecret(); !persisted {
		ctx.Logger.Info("Waiting for ssh keys data secret to be created by KubevirtCluster controller...")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	if err := clusterNodeSshKeys.FetchPersistedKeysFromSecret(); err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to fetch ssh keys for cluster nodes")
	}

	if err := r.reconcileKubevirtBootstrapSecret(ctx, clusterNodeSshKeys); err != nil {
		ctx.Logger.Info("Waiting for the Bootstrap provider controller to set bootstrap data")
		conditions.MarkFalse(ctx.KubevirtMachine, infrav1.VMProvisionedCondition, infrav1.WaitingForBootstrapDataReason, clusterv1.ConditionSeverityInfo, "")
		return ctrl.Result{}, nil
	}

	// Create a helper for managing the KubeVirt VM hosting the machine.
	externalMachine, err := kubevirthandler.NewMachine(ctx, r.Client)
	if err != nil {
		return ctrl.Result{}, errors.Wrapf(err, "failed to create helper for managing the externalMachine")
	}

	// Provision the underlying VM if not existing
	if !externalMachine.Exists() {
		ctx.Logger.Info("Creating underlying VM instance...")
		if err := externalMachine.Create(); err != nil {
			return ctrl.Result{}, errors.Wrap(err, "failed to create VM instance")
		}
	}

	vmCommandExecutor := ssh.VMCommandExecutor{
		IPAddress:  externalMachine.Address(),
		PublicKey:  clusterNodeSshKeys.PublicKey,
		PrivateKey: clusterNodeSshKeys.PrivateKey,
	}

	// Wait for VM to boot
	if !externalMachine.IsBooted(vmCommandExecutor) {
		ctx.Logger.Info("Waiting for underlying VM instance to boot...")
		return ctrl.Result{RequeueAfter: 20 * time.Second}, nil
	}

	// Update the VMProvisionedCondition condition
	// NOTE: it is required to create the patch helper at this point, otherwise it won't surface if we issue a patch down in the code
	// (because if we create patch helper after this point the VMProvisionedCondition=True exists both on before and after).
	patchHelper, err := patch.NewHelper(ctx.KubevirtMachine, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}
	conditions.MarkTrue(ctx.KubevirtMachine, infrav1.VMProvisionedCondition)

	// At, this stage, we are ready for bootstrap. However, if the BootstrapExecSucceededCondition is missing we add it and we
	// issue an patch so the user can see the change of state before the bootstrap actually starts.
	// NOTE: usually controller should not rely on status they are setting, but on the observed state; however
	// in this case we are doing this because we explicitly want to give a feedback to users.
	if !conditions.Has(ctx.KubevirtMachine, infrav1.BootstrapExecSucceededCondition) {
		conditions.MarkFalse(ctx.KubevirtMachine, infrav1.BootstrapExecSucceededCondition, infrav1.BootstrappingReason, clusterv1.ConditionSeverityInfo, "")
		if err := ctx.PatchKubevirtMachine(patchHelper); err != nil {
			return ctrl.Result{}, errors.Wrap(err, "failed to patch KubevirtMachine")
		}
	}

	// Wait for VM to bootstrap with Kubernetes
	if !ctx.KubevirtMachine.Spec.Bootstrapped {
		if !externalMachine.IsBootstrapped(vmCommandExecutor) {
			ctx.Logger.Info("Waiting for underlying VM to bootstrap...")
			conditions.MarkFalse(ctx.KubevirtMachine, infrav1.BootstrapExecSucceededCondition, infrav1.BootstrapFailedReason, clusterv1.ConditionSeverityWarning, "VM not bootstrapped yet")
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}

		ctx.KubevirtMachine.Spec.Bootstrapped = true
	}

	// Update the condition BootstrapExecSucceededCondition
	conditions.MarkTrue(ctx.KubevirtMachine, infrav1.BootstrapExecSucceededCondition)

	ctx.KubevirtMachine.Status.Addresses = []clusterv1.MachineAddress{
		{
			Type:    clusterv1.MachineHostName,
			Address: ctx.KubevirtMachine.Name,
		},
		{
			Type:    clusterv1.MachineInternalIP,
			Address: externalMachine.Address(),
		},
		{
			Type:    clusterv1.MachineExternalIP,
			Address: externalMachine.Address(),
		},
	}

	// Patch node with provider id.
	// Usually a cloud provider will do this, but there is no cloud provider for KubeVirt.
	ctx.Logger.Info("Patching node with provider id...")
	var providerID string

	workloadClusterClient, err := r.reconcileWorkloadClusterClient(ctx)
	if err != nil {
		ctx.Logger.Error(err, "Workload cluster client is not available")
	}
	if workloadClusterClient == nil {
		ctx.Logger.Info("Waiting for workload cluster client...")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil

	}

	if providerID, err = externalMachine.SetProviderID(workloadClusterClient); err != nil {
		ctx.Logger.Error(err, "Failed to patch node with provider id...")
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Set ProviderID so the Cluster API Machine Controller can pull it.
	ctx.KubevirtMachine.Spec.ProviderID = &providerID

	// KubevirtMachine is ready! Set the status and the condition.
	ctx.KubevirtMachine.Status.Ready = true
	conditions.MarkTrue(ctx.KubevirtMachine, infrav1.VMProvisionedCondition)

	return ctrl.Result{}, nil
}

func (r *KubevirtMachineReconciler) reconcileDelete(ctx *context.MachineContext) (ctrl.Result, error) {
	// Set the VMProvisionedCondition reporting delete is started, and issue a patch in order to make
	// this visible to the users.
	patchHelper, err := patch.NewHelper(ctx.KubevirtMachine, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}
	conditions.MarkFalse(ctx.KubevirtMachine, infrav1.VMProvisionedCondition, clusterv1.DeletingReason, clusterv1.ConditionSeverityInfo, "")
	if err := ctx.PatchKubevirtMachine(patchHelper); err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to patch KubevirtMachine")
	}

	// Machine is deleted so remove the finalizer.
	controllerutil.RemoveFinalizer(ctx.KubevirtMachine, infrav1.MachineFinalizer)

	return ctrl.Result{}, nil
}

// SetupWithManager will add watches for this controller.
func (r *KubevirtMachineReconciler) SetupWithManager(goctx gocontext.Context, mgr ctrl.Manager, options controller.Options) error {
	clusterToKubevirtMachines, err := util.ClusterToObjectsMapper(mgr.GetClient(), &infrav1.KubevirtMachineList{}, mgr.GetScheme())
	if err != nil {
		return err
	}

	c, err := ctrl.NewControllerManagedBy(mgr).
		For(&infrav1.KubevirtMachine{}).
		WithOptions(options).
		WithEventFilter(predicates.ResourceNotPaused(ctrl.LoggerFrom(goctx))).
		Watches(
			&source.Kind{Type: &clusterv1.Machine{}},
			handler.EnqueueRequestsFromMapFunc(util.MachineToInfrastructureMapFunc(infrav1.GroupVersion.WithKind("KubevirtMachine"))),
		).
		Watches(
			&source.Kind{Type: &infrav1.KubevirtCluster{}},
			handler.EnqueueRequestsFromMapFunc(r.KubevirtClusterToKubevirtMachines),
		).
		Build(r)
	if err != nil {
		return err
	}
	return c.Watch(
		&source.Kind{Type: &clusterv1.Cluster{}},
		handler.EnqueueRequestsFromMapFunc(clusterToKubevirtMachines),
		predicates.ClusterUnpausedAndInfrastructureReady(ctrl.LoggerFrom(goctx)),
	)
}

// KubevirtClusterToKubevirtMachines is a handler.ToRequestsFunc to be used to enqueue
// requests for reconciliation of KubevirtMachines.
func (r *KubevirtMachineReconciler) KubevirtClusterToKubevirtMachines(o client.Object) []ctrl.Request {
	var result []ctrl.Request
	c, ok := o.(*infrav1.KubevirtCluster)
	if !ok {
		panic(fmt.Sprintf("Expected a KubevirtCluster but got a %T", o))
	}

	cluster, err := util.GetOwnerCluster(gocontext.TODO(), r.Client, c.ObjectMeta)
	switch {
	case apierrors.IsNotFound(err) || cluster == nil:
		return result
	case err != nil:
		return result
	}

	labels := map[string]string{clusterv1.ClusterLabelName: cluster.Name}
	machineList := &clusterv1.MachineList{}
	if err := r.Client.List(gocontext.TODO(), machineList, client.InNamespace(c.Namespace), client.MatchingLabels(labels)); err != nil {
		return nil
	}
	for _, m := range machineList.Items {
		if m.Spec.InfrastructureRef.Name == "" {
			continue
		}
		name := client.ObjectKey{Namespace: m.Namespace, Name: m.Name}
		result = append(result, ctrl.Request{NamespacedName: name})
	}

	return result
}

// reconcileKubevirtBootstrapSecret creates bootstrap cloud-init secret for KubeVirt virtual machines
func (r *KubevirtMachineReconciler) reconcileKubevirtBootstrapSecret(ctx *context.MachineContext, sshKeys *ssh.ClusterNodeSshKeys) error {
	if ctx.Machine.Spec.Bootstrap.DataSecretName == nil {
		return errors.New("error retrieving bootstrap data: linked Machine's bootstrap.dataSecretName is nil")
	}

	// Exit early if exists.
	bootstrapDataSecret := &corev1.Secret{}
	bootstrapDataSecretKey := client.ObjectKey{Namespace: ctx.Machine.GetNamespace(), Name: *ctx.Machine.Spec.Bootstrap.DataSecretName + "-userdata"}
	if err := r.Client.Get(ctx, bootstrapDataSecretKey, bootstrapDataSecret); err == nil {
		return nil
	}

	s := &corev1.Secret{}
	key := client.ObjectKey{Namespace: ctx.Machine.GetNamespace(), Name: *ctx.Machine.Spec.Bootstrap.DataSecretName}
	if err := r.Client.Get(ctx, key, s); err != nil {
		return errors.Wrapf(err, "failed to retrieve bootstrap data secret for KubevirtMachine %s/%s", ctx.Machine.GetNamespace(), ctx.Machine.GetName())
	}

	value, ok := s.Data["value"]
	if !ok {
		return errors.New("error retrieving bootstrap data: secret value key is missing")
	}

	//updatedValue, err := updateUserDataUsers(ctx, sshKeys.PublicKey, []byte(value))
	//if err != nil {
	//	return errors.Wrapf(err, "failed to update users (userdata) for for KubevirtMachine %s/%s", ctx.Machine.GetNamespace(), ctx.Machine.GetName())
	//}
	updatedValue := value
	newBootstrapDataSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      s.Name + "-userdata",
			Namespace: ctx.Machine.GetNamespace(),
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, newBootstrapDataSecret, func() error {
		newBootstrapDataSecret.Type = clusterv1.ClusterSecretType
		newBootstrapDataSecret.Data = map[string][]byte{
			"userdata": updatedValue,
		}

		// set owner reference for secret
		mutateFn := func() (err error) {
			newBootstrapDataSecret.SetOwnerReferences(clusterutil.EnsureOwnerRef(
				newBootstrapDataSecret.OwnerReferences,
				metav1.OwnerReference{
					APIVersion: ctx.KubevirtMachine.APIVersion,
					Kind:       ctx.KubevirtMachine.Kind,
					Name:       ctx.KubevirtMachine.Name,
					UID:        ctx.KubevirtMachine.UID,
				}))
			return nil
		}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, newBootstrapDataSecret, mutateFn); err != nil {
			return errors.Wrapf(err, "failed to set owner reference for secret")
		}

		return nil
	})

	if err != nil {
		return errors.Wrapf(err, "failed to create kubevirt bootstrap secret for cluster")
	}

	return nil
}

// getKubeconfigForWorkloadCluster fetches kubeconfig for workload cluster from the corresponding secret.
func (r *KubevirtMachineReconciler) getKubeconfigForWorkloadCluster(ctx *context.MachineContext) (string, error) {
	// workload cluster kubeconfig can be found in a secret with suffix "-kubeconfig"
	kubeconfigSecret := &corev1.Secret{}
	kubeconfigSecretKey := client.ObjectKey{Namespace: ctx.KubevirtCluster.Namespace, Name: ctx.KubevirtCluster.Name + "-kubeconfig"}
	if err := r.Client.Get(ctx, kubeconfigSecretKey, kubeconfigSecret); err != nil {
		return "", errors.Wrapf(err, "failed to fetch kubeconfig for workload cluster")
	}

	// read kubeconfig
	value, ok := kubeconfigSecret.Data["value"]
	if !ok {
		return "", errors.New("error retrieving kubeconfig data: secret value key is missing")
	}

	return string(value), nil
}

// reconcileWorkloadClusterClient creates a client for workload cluster.
func (r *KubevirtMachineReconciler) reconcileWorkloadClusterClient(ctx *context.MachineContext) (client.Client, error) {
	// get workload cluster kubeconfig
	kubeConfig, err := r.getKubeconfigForWorkloadCluster(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get kubeconfig for workload cluster")
	}

	// generate REST config
	restConfig, err := clientcmd.RESTConfigFromKubeConfig([]byte(kubeConfig))
	if err != nil {
		return nil, errors.Wrap(err, "failed to create REST config")
	}

	// create the client
	workloadClusterClient, err := client.New(restConfig, client.Options{Scheme: r.Client.Scheme()})
	if err != nil {
		return nil, errors.Wrap(err, "failed to create workload cluster client")
	}

	return workloadClusterClient, nil
}

func updateUserDataUsers(ctx *context.MachineContext, sshPublicKey []byte, src []byte) ([]byte, error) {
	var dataMap map[string]interface{}
	if err := json.Unmarshal(src, &dataMap); err == nil {
		if _, ok := dataMap["passwd"]; !ok {
			dataMap["passwd"] = map[string]interface{}{}
		}
		passwd := (dataMap["passwd"]).(map[string]interface{})

		if _, ok := passwd["users"]; !ok {
			passwd["users"] = []map[string]interface{}{}
		}
		newUser := map[string]interface{}{
			"name":              "core",
			"sshAuthorizedKeys": sshPublicKey,
		}
		passwd["users"] = append(passwd["users"].([]map[string]interface{}), newUser)
		result, err := json.Marshal(dataMap)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to update user data (ignition) for KubevirtMachine %s/%s", ctx.Machine.GetNamespace(), ctx.Machine.GetName())
		}
		return result, nil
	} else {
		ctx.Logger.Info("Adding users config to bootstrap data...")
		return []byte(string(src) + usersCloudConfig(sshPublicKey)), nil
	}
}

// usersCloudConfig generates 'users' cloud config for capk user with a given ssh public key
func usersCloudConfig(sshPublicKey []byte) string {
	sshPublicKeyString := base64.StdEncoding.EncodeToString(sshPublicKey)
	sshPublicKeyDecoded, _ := base64.StdEncoding.DecodeString(sshPublicKeyString)

	return `users:
  - name: capk
    gecos: CAPK User
    sudo: ALL=(ALL) NOPASSWD:ALL
    groups: users, admin
    ssh_authorized_keys:
      - ` + string(sshPublicKeyDecoded)
}
