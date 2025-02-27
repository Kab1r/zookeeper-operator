/**
 * Copyright (c) 2018 Dell Inc., or its subsidiaries. All Rights Reserved.
 *
 * Licensed under the Apache License, Version 2.0 (&the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 */
package controllers

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/trace"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/pravega/zookeeper-operator/pkg/controller/config"
	"github.com/pravega/zookeeper-operator/pkg/utils"
	"github.com/pravega/zookeeper-operator/pkg/yamlexporter"
	"github.com/pravega/zookeeper-operator/pkg/zk"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	zookeeperv1beta1 "github.com/pravega/zookeeper-operator/api/v1beta1"
)

// ReconcileTime is the delay between reconciliations
const ReconcileTime = 30 * time.Second

var log = logf.Log.WithName("controller_zookeepercluster")

var _ reconcile.Reconciler = &ZookeeperClusterReconciler{}

// ZookeeperClusterReconciler reconciles a ZookeeperCluster object
type ZookeeperClusterReconciler struct {
	Client   client.Client
	Log      logr.Logger
	Scheme   *runtime.Scheme
	ZkClient zk.ZookeeperClient
	Tracer   trace.Tracer
}

type reconcileFun func(ctx context.Context, cluster *zookeeperv1beta1.ZookeeperCluster) error

// +kubebuilder:rbac:groups=zookeeper.pravega.io.zookeeper.pravega.io,resources=zookeeperclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=zookeeper.pravega.io.zookeeper.pravega.io,resources=zookeeperclusters/status,verbs=get;update;patch

func (r *ZookeeperClusterReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	ctx, span := r.Tracer.Start(ctx, "Reconcile")
	defer span.End()
	r.Log = log.WithValues(
		"Request.Namespace", request.Namespace,
		"Request.Name", request.Name)
	r.Log.Info("Reconciling ZookeeperCluster")

	// Fetch the ZookeeperCluster instance
	instance := &zookeeperv1beta1.ZookeeperCluster{}
	err := r.Client.Get(ctx, request.NamespacedName, instance)
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile
			// request. Owned objects are automatically garbage collected. For
			// additional cleanup logic use finalizers.
			// Return and don't requeue
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}
	changed := instance.WithDefaults()
	if instance.GetTriggerRollingRestart() {
		r.Log.Info("Restarting zookeeper cluster")
		annotationkey, annotationvalue := getRollingRestartAnnotation()
		if instance.Spec.Pod.Annotations == nil {
			instance.Spec.Pod.Annotations = make(map[string]string)
		}
		instance.Spec.Pod.Annotations[annotationkey] = annotationvalue
		instance.SetTriggerRollingRestart(false)
		changed = true
	}
	if changed {
		r.Log.Info("Setting default settings for zookeeper-cluster")
		if err := r.Client.Update(ctx, instance); err != nil {
			return reconcile.Result{}, err
		}
		return reconcile.Result{Requeue: true}, nil
	}
	for _, fun := range []reconcileFun{
		r.reconcileFinalizers,
		r.reconcileConfigMap,
		r.reconcileStatefulSet,
		r.reconcileClientService,
		r.reconcileHeadlessService,
		r.reconcileAdminServerService,
		r.reconcilePodDisruptionBudget,
		r.reconcileClusterStatus,
	} {
		if err = fun(ctx, instance); err != nil {
			return reconcile.Result{}, err
		}
	}
	// Recreate any missing resources every 'ReconcileTime'
	return reconcile.Result{RequeueAfter: ReconcileTime}, nil
}

func getRollingRestartAnnotation() (string, string) {
	return "restartTime", time.Now().Format(time.RFC850)
}

// compareResourceVersion compare resoure versions for the supplied ZookeeperCluster and StatefulSet
// resources
// Returns:
// 0 if versions are equal
// -1 if ZookeeperCluster version is less than StatefulSet version
// 1 if ZookeeperCluster version is greater than StatefulSet version
func compareResourceVersion(zk *zookeeperv1beta1.ZookeeperCluster, sts *appsv1.StatefulSet) int {

	zkResourceVersion, zkErr := strconv.Atoi(zk.ResourceVersion)
	stsVersion, stsVersionFound := sts.Labels["owner-rv"]

	if !stsVersionFound {
		if zkErr != nil {
			log.Info("Fail to parse ZookeeperCluster version. Cannot decide zookeeper StatefulSet version")
			return 0
		}
		return 1
	}
	stsResourceVersion, err := strconv.Atoi(stsVersion)
	if err != nil {
		if zkErr != nil {
			log.Info("Fail to parse ZookeeperCluster version. Cannot decide zookeeper StatefulSet version")
			return 0
		}
		log.Info("Fail to convert StatefulSet version %s to integer; setting it to ZookeeperCluster version", stsVersion)
		return 1
	}
	if zkResourceVersion < stsResourceVersion {
		return -1
	} else if zkResourceVersion > stsResourceVersion {
		return 1
	}
	return 0
}

func (r *ZookeeperClusterReconciler) reconcileStatefulSet(ctx context.Context, instance *zookeeperv1beta1.ZookeeperCluster) (err error) {
	ctx, span := r.Tracer.Start(ctx, "reconcileStatefulSet")
	defer span.End()

	// we cannot upgrade if cluster is in UpgradeFailed
	if instance.Status.IsClusterInUpgradeFailedState() {
		sts := zk.MakeStatefulSet(instance)
		if err = controllerutil.SetControllerReference(instance, sts, r.Scheme); err != nil {
			return err
		}
		foundSts := &appsv1.StatefulSet{}
		err = r.Client.Get(ctx, types.NamespacedName{
			Name:      sts.Name,
			Namespace: sts.Namespace,
		}, foundSts)
		if err == nil {
			err = r.Client.Update(ctx, foundSts)
			if err != nil {
				return err
			}
			if foundSts.Status.Replicas == foundSts.Status.ReadyReplicas && foundSts.Status.CurrentRevision == foundSts.Status.UpdateRevision {
				r.Log.Info("failed upgrade completed", "upgrade from:", instance.Status.CurrentVersion, "upgrade to:", instance.Status.TargetVersion)
				instance.Status.CurrentVersion = instance.Status.TargetVersion
				instance.Status.SetErrorConditionFalse()
				return r.clearUpgradeStatus(ctx, instance)
			} else {
				r.Log.Info("Unable to recover failed upgrade, make sure all nodes are running the target version")
			}

		}
	}

	if instance.Status.IsClusterInUpgradeFailedState() {
		return nil
	}
	if instance.Spec.Pod.ServiceAccountName != "default" {
		serviceAccount := zk.MakeServiceAccount(instance)
		if err = controllerutil.SetControllerReference(instance, serviceAccount, r.Scheme); err != nil {
			return err
		}
		// Check if this ServiceAccount already exists
		foundServiceAccount := &corev1.ServiceAccount{}
		err = r.Client.Get(ctx, types.NamespacedName{Name: serviceAccount.Name, Namespace: serviceAccount.Namespace}, foundServiceAccount)
		if err != nil && errors.IsNotFound(err) {
			r.Log.Info("Creating a new ServiceAccount", "ServiceAccount.Namespace", serviceAccount.Namespace, "ServiceAccount.Name", serviceAccount.Name)
			err = r.Client.Create(ctx, serviceAccount)
			if err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else {
			foundServiceAccount.ImagePullSecrets = serviceAccount.ImagePullSecrets
			r.Log.Info("Updating ServiceAccount", "ServiceAccount.Namespace", serviceAccount.Namespace, "ServiceAccount.Name", serviceAccount.Name)
			err = r.Client.Update(ctx, foundServiceAccount)
			if err != nil {
				return err
			}
		}
	}
	sts := zk.MakeStatefulSet(instance)
	if err = controllerutil.SetControllerReference(instance, sts, r.Scheme); err != nil {
		return err
	}
	foundSts := &appsv1.StatefulSet{}
	err = r.Client.Get(ctx, types.NamespacedName{
		Name:      sts.Name,
		Namespace: sts.Namespace,
	}, foundSts)
	if err != nil && errors.IsNotFound(err) {
		r.Log.Info("Creating a new Zookeeper StatefulSet",
			"StatefulSet.Namespace", sts.Namespace,
			"StatefulSet.Name", sts.Name)
		// label the RV of the zookeeperCluster when creating the sts
		sts.Labels["owner-rv"] = instance.ResourceVersion
		err = r.Client.Create(ctx, sts)
		if err != nil {
			return err
		}
		return nil
	} else if err != nil {
		return err
	} else {
		// check whether zookeeperCluster is updated before updating the sts
		cmp := compareResourceVersion(instance, foundSts)
		if cmp < 0 {
			return fmt.Errorf("Staleness: cr.ResourceVersion %s is smaller than labeledRV %s", instance.ResourceVersion, foundSts.Labels["owner-rv"])
		} else if cmp > 0 {
			// Zookeeper StatefulSet version inherits ZookeeperCluster resource version
			foundSts.Labels["owner-rv"] = instance.ResourceVersion
		}
		foundSTSSize := *foundSts.Spec.Replicas
		newSTSSize := *sts.Spec.Replicas
		if newSTSSize != foundSTSSize {
			zkUri := utils.GetZkServiceUri(instance)
			err = r.ZkClient.Connect(zkUri)
			if err != nil {
				return fmt.Errorf("Error storing cluster size %v", err)
			}
			defer r.ZkClient.Close()
			r.Log.Info("Connected to ZK", "ZKURI", zkUri)

			path := utils.GetMetaPath(instance)
			version, err := r.ZkClient.NodeExists(path)
			if err != nil {
				return fmt.Errorf("Error doing exists check for znode %s: %v", path, err)
			}

			data := "CLUSTER_SIZE=" + strconv.Itoa(int(newSTSSize))
			r.Log.Info("Updating Cluster Size.", "New Data:", data, "Version", version)
			r.ZkClient.UpdateNode(path, data, version)
		}
		err = r.updateStatefulSet(ctx, instance, foundSts, sts)
		if err != nil {
			return err
		}
		return r.upgradeStatefulSet(ctx, instance, foundSts)
	}
}

func (r *ZookeeperClusterReconciler) updateStatefulSet(ctx context.Context, instance *zookeeperv1beta1.ZookeeperCluster, foundSts *appsv1.StatefulSet, sts *appsv1.StatefulSet) (err error) {
	r.Log.Info("Updating StatefulSet",
		"StatefulSet.Namespace", foundSts.Namespace,
		"StatefulSet.Name", foundSts.Name)
	zk.SyncStatefulSet(foundSts, sts)

	err = r.Client.Update(ctx, foundSts)
	if err != nil {
		return err
	}
	instance.Status.Replicas = foundSts.Status.Replicas
	instance.Status.ReadyReplicas = foundSts.Status.ReadyReplicas
	return nil
}

func (r *ZookeeperClusterReconciler) upgradeStatefulSet(ctx context.Context, instance *zookeeperv1beta1.ZookeeperCluster, foundSts *appsv1.StatefulSet) (err error) {

	// Getting the upgradeCondition from the zk clustercondition
	_, upgradeCondition := instance.Status.GetClusterCondition(zookeeperv1beta1.ClusterConditionUpgrading)

	if upgradeCondition == nil {
		// Initially set upgrading condition to false
		instance.Status.SetUpgradingConditionFalse()
		return nil
	}

	// Setting the upgrade condition to true to trigger the upgrade
	// When the zk cluster is upgrading Statefulset CurrentRevision and UpdateRevision are not equal and zk cluster image tag is not equal to CurrentVersion
	if upgradeCondition.Status == corev1.ConditionFalse {
		if instance.Status.IsClusterInReadyState() && foundSts.Status.CurrentRevision != foundSts.Status.UpdateRevision && instance.Spec.Image.Tag != instance.Status.CurrentVersion {
			instance.Status.TargetVersion = instance.Spec.Image.Tag
			instance.Status.SetPodsReadyConditionFalse()
			instance.Status.SetUpgradingConditionTrue("", "")
		}
	}

	// checking if the upgrade is in progress
	if upgradeCondition.Status == corev1.ConditionTrue {
		// checking when the targetversion is empty
		if instance.Status.TargetVersion == "" {
			r.Log.Info("upgrading to an unknown version: cancelling upgrade process")
			return r.clearUpgradeStatus(ctx, instance)
		}
		// Checking for upgrade completion
		if foundSts.Status.CurrentRevision == foundSts.Status.UpdateRevision {
			instance.Status.CurrentVersion = instance.Status.TargetVersion
			r.Log.Info("upgrade completed")
			return r.clearUpgradeStatus(ctx, instance)
		}
		// updating the upgradecondition if upgrade is in progress
		if foundSts.Status.CurrentRevision != foundSts.Status.UpdateRevision {
			r.Log.Info("upgrade in progress")
			if fmt.Sprint(foundSts.Status.UpdatedReplicas) != upgradeCondition.Message {
				instance.Status.UpdateProgress(zookeeperv1beta1.UpdatingZookeeperReason, fmt.Sprint(foundSts.Status.UpdatedReplicas))
			} else {
				err = checkSyncTimeout(instance, zookeeperv1beta1.UpdatingZookeeperReason, foundSts.Status.UpdatedReplicas, 10*time.Minute)
				if err != nil {
					instance.Status.SetErrorConditionTrue("UpgradeFailed", err.Error())
					return r.Client.Status().Update(ctx, instance)
				} else {
					return nil
				}
			}
		}
	}
	return r.Client.Status().Update(ctx, instance)
}

func (r *ZookeeperClusterReconciler) clearUpgradeStatus(ctx context.Context, z *zookeeperv1beta1.ZookeeperCluster) (err error) {
	z.Status.SetUpgradingConditionFalse()
	z.Status.TargetVersion = ""
	// need to deep copy the status struct, otherwise it will be overwritten
	// when updating the CR below
	status := z.Status.DeepCopy()

	err = r.Client.Update(ctx, z)
	if err != nil {
		return err
	}

	z.Status = *status
	return nil
}

func checkSyncTimeout(z *zookeeperv1beta1.ZookeeperCluster, reason string, updatedReplicas int32, t time.Duration) error {
	lastCondition := z.Status.GetLastCondition()
	if lastCondition == nil {
		return nil
	}
	if lastCondition.Reason == reason && lastCondition.Message == fmt.Sprint(updatedReplicas) {
		// if reason and message are the same as before, which means there is no progress since the last reconciling,
		// then check if it reaches the timeout.
		parsedTime, _ := time.Parse(time.RFC3339, lastCondition.LastUpdateTime)
		if time.Now().After(parsedTime.Add(t)) {
			// timeout
			return fmt.Errorf("progress deadline exceeded")
		}
	}
	return nil
}

func (r *ZookeeperClusterReconciler) reconcileClientService(ctx context.Context, instance *zookeeperv1beta1.ZookeeperCluster) (err error) {
	ctx, span := r.Tracer.Start(ctx, "reconcileClientService")
	defer span.End()
	svc := zk.MakeClientService(instance)
	if err = controllerutil.SetControllerReference(instance, svc, r.Scheme); err != nil {
		return err
	}
	foundSvc := &corev1.Service{}
	err = r.Client.Get(ctx, types.NamespacedName{
		Name:      svc.Name,
		Namespace: svc.Namespace,
	}, foundSvc)
	if err != nil && errors.IsNotFound(err) {
		r.Log.Info("Creating new client service",
			"Service.Namespace", svc.Namespace,
			"Service.Name", svc.Name)
		err = r.Client.Create(ctx, svc)
		if err != nil {
			return err
		}
		return nil
	} else if err != nil {
		return err
	} else {
		r.Log.Info("Updating existing client service",
			"Service.Namespace", foundSvc.Namespace,
			"Service.Name", foundSvc.Name)
		zk.SyncService(foundSvc, svc)
		err = r.Client.Update(ctx, foundSvc)
		if err != nil {
			return err
		}
		port := instance.ZookeeperPorts().Client
		instance.Status.InternalClientEndpoint = fmt.Sprintf("%s:%d",
			foundSvc.Spec.ClusterIP, port)
		if foundSvc.Spec.Type == "LoadBalancer" {
			for _, i := range foundSvc.Status.LoadBalancer.Ingress {
				if i.IP != "" {
					instance.Status.ExternalClientEndpoint = fmt.Sprintf("%s:%d",
						i.IP, port)
				}
			}
		} else {
			instance.Status.ExternalClientEndpoint = "N/A"
		}
	}
	return nil
}

func (r *ZookeeperClusterReconciler) reconcileHeadlessService(ctx context.Context, instance *zookeeperv1beta1.ZookeeperCluster) (err error) {
	ctx, span := r.Tracer.Start(ctx, "reconcileHeadlessService")
	defer span.End()
	svc := zk.MakeHeadlessService(instance)
	if err = controllerutil.SetControllerReference(instance, svc, r.Scheme); err != nil {
		return err
	}
	foundSvc := &corev1.Service{}
	err = r.Client.Get(ctx, types.NamespacedName{
		Name:      svc.Name,
		Namespace: svc.Namespace,
	}, foundSvc)
	if err != nil && errors.IsNotFound(err) {
		r.Log.Info("Creating new headless service",
			"Service.Namespace", svc.Namespace,
			"Service.Name", svc.Name)
		err = r.Client.Create(ctx, svc)
		if err != nil {
			return err
		}
		return nil
	} else if err != nil {
		return err
	} else {
		r.Log.Info("Updating existing headless service",
			"Service.Namespace", foundSvc.Namespace,
			"Service.Name", foundSvc.Name)
		zk.SyncService(foundSvc, svc)
		err = r.Client.Update(ctx, foundSvc)
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *ZookeeperClusterReconciler) reconcileAdminServerService(ctx context.Context, instance *zookeeperv1beta1.ZookeeperCluster) (err error) {
	ctx, span := r.Tracer.Start(ctx, "reconcileAdminServerService")
	defer span.End()
	svc := zk.MakeAdminServerService(instance)
	if err = controllerutil.SetControllerReference(instance, svc, r.Scheme); err != nil {
		return err
	}
	foundSvc := &corev1.Service{}
	err = r.Client.Get(ctx, types.NamespacedName{
		Name:      svc.Name,
		Namespace: svc.Namespace,
	}, foundSvc)
	if err != nil && errors.IsNotFound(err) {
		r.Log.Info("Creating admin server service",
			"Service.Namespace", svc.Namespace,
			"Service.Name", svc.Name)
		err = r.Client.Create(ctx, svc)
		if err != nil {
			return err
		}
		return nil
	} else if err != nil {
		return err
	} else {
		r.Log.Info("Updating existing admin server service",
			"Service.Namespace", foundSvc.Namespace,
			"Service.Name", foundSvc.Name)
		zk.SyncService(foundSvc, svc)
		err = r.Client.Update(ctx, foundSvc)
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *ZookeeperClusterReconciler) reconcilePodDisruptionBudget(ctx context.Context, instance *zookeeperv1beta1.ZookeeperCluster) (err error) {
	ctx, span := r.Tracer.Start(ctx, "reconcilePodDisruptionBudget")
	defer span.End()
	pdb := zk.MakePodDisruptionBudget(instance)
	if err = controllerutil.SetControllerReference(instance, pdb, r.Scheme); err != nil {
		return err
	}
	foundPdb := &policyv1.PodDisruptionBudget{}
	err = r.Client.Get(ctx, types.NamespacedName{
		Name:      pdb.Name,
		Namespace: pdb.Namespace,
	}, foundPdb)
	if err != nil && errors.IsNotFound(err) {
		r.Log.Info("Creating new pod-disruption-budget",
			"PodDisruptionBudget.Namespace", pdb.Namespace,
			"PodDisruptionBudget.Name", pdb.Name)
		err = r.Client.Create(ctx, pdb)
		if err != nil {
			return err
		}
		return nil
	} else if err != nil {
		return err
	}
	return nil
}

func (r *ZookeeperClusterReconciler) reconcileConfigMap(ctx context.Context, instance *zookeeperv1beta1.ZookeeperCluster) (err error) {
	ctx, span := r.Tracer.Start(ctx, "reconcileConfigMap")
	defer span.End()
	cm := zk.MakeConfigMap(instance)
	if err = controllerutil.SetControllerReference(instance, cm, r.Scheme); err != nil {
		return err
	}
	foundCm := &corev1.ConfigMap{}
	err = r.Client.Get(ctx, types.NamespacedName{
		Name:      cm.Name,
		Namespace: cm.Namespace,
	}, foundCm)
	if err != nil && errors.IsNotFound(err) {
		r.Log.Info("Creating a new Zookeeper Config Map",
			"ConfigMap.Namespace", cm.Namespace,
			"ConfigMap.Name", cm.Name)
		err = r.Client.Create(ctx, cm)
		if err != nil {
			return err
		}
		return nil
	} else if err != nil {
		return err
	} else {
		r.Log.Info("Updating existing config-map",
			"ConfigMap.Namespace", foundCm.Namespace,
			"ConfigMap.Name", foundCm.Name)
		zk.SyncConfigMap(foundCm, cm)
		err = r.Client.Update(ctx, foundCm)
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *ZookeeperClusterReconciler) reconcileClusterStatus(ctx context.Context, instance *zookeeperv1beta1.ZookeeperCluster) (err error) {
	ctx, span := r.Tracer.Start(ctx, "reconcileClusterStatus")
	defer span.End()
	if instance.Status.IsClusterInUpgradingState() || instance.Status.IsClusterInUpgradeFailedState() {
		return nil
	}
	instance.Status.Init()
	foundPods := &corev1.PodList{}
	labelSelector := labels.SelectorFromSet(map[string]string{"app": instance.GetName()})
	listOps := &client.ListOptions{
		Namespace:     instance.Namespace,
		LabelSelector: labelSelector,
	}
	err = r.Client.List(ctx, foundPods, listOps)
	if err != nil {
		return err
	}
	var (
		readyMembers   []string
		unreadyMembers []string
	)
	for _, p := range foundPods.Items {
		ready := true
		for _, c := range p.Status.ContainerStatuses {
			if !c.Ready {
				ready = false
			}
		}
		if ready {
			readyMembers = append(readyMembers, p.Name)
		} else {
			unreadyMembers = append(unreadyMembers, p.Name)
		}
	}
	instance.Status.Members.Ready = readyMembers
	instance.Status.Members.Unready = unreadyMembers

	// If Cluster is in a ready state...
	if instance.Spec.Replicas == instance.Status.ReadyReplicas && (!instance.Status.MetaRootCreated) {
		r.Log.Info("Cluster is Ready, Creating ZK Metadata...")
		zkUri := utils.GetZkServiceUri(instance)
		err := r.ZkClient.Connect(zkUri)
		if err != nil {
			return fmt.Errorf("Error creating cluster metaroot. Connect to zk failed %v", err)
		}
		defer r.ZkClient.Close()
		metaPath := utils.GetMetaPath(instance)
		r.Log.Info("Connected to zookeeper:", "ZKUri", zkUri, "Creating Path", metaPath)
		if err := r.ZkClient.CreateNode(instance, metaPath); err != nil {
			return fmt.Errorf("Error creating cluster metadata path %s, %v", metaPath, err)
		}
		r.Log.Info("Metadata znode created.")
		instance.Status.MetaRootCreated = true
	}
	r.Log.Info("Updating zookeeper status",
		"StatefulSet.Namespace", instance.Namespace,
		"StatefulSet.Name", instance.Name)
	if instance.Status.ReadyReplicas == instance.Spec.Replicas {
		instance.Status.SetPodsReadyConditionTrue()
	} else {
		instance.Status.SetPodsReadyConditionFalse()
	}
	if instance.Status.CurrentVersion == "" && instance.Status.IsClusterInReadyState() {
		instance.Status.CurrentVersion = instance.Spec.Image.Tag
	}
	return r.Client.Status().Update(ctx, instance)
}

// YAMLExporterReconciler returns a fake Reconciler which is being used for generating YAML files
func YAMLExporterReconciler(zookeepercluster *zookeeperv1beta1.ZookeeperCluster) *ZookeeperClusterReconciler {
	var scheme = scheme.Scheme
	scheme.AddKnownTypes(zookeeperv1beta1.GroupVersion, zookeepercluster)
	return &ZookeeperClusterReconciler{
		Client:   fake.NewClientBuilder().WithRuntimeObjects(zookeepercluster).Build(),
		Scheme:   scheme,
		ZkClient: new(zk.DefaultZookeeperClient),
	}
}

// GenerateYAML generated secondary resource of ZookeeperCluster resources YAML files
func (r *ZookeeperClusterReconciler) GenerateYAML(inst *zookeeperv1beta1.ZookeeperCluster) error {
	if inst.WithDefaults() {
		fmt.Println("set default values")
	}
	for _, fun := range []reconcileFun{
		r.yamlConfigMap,
		r.yamlStatefulSet,
		r.yamlClientService,
		r.yamlHeadlessService,
		r.yamlPodDisruptionBudget,
	} {
		ctx := context.TODO()
		if err := fun(ctx, inst); err != nil {
			return err
		}
	}
	return nil
}

// yamlStatefulSet will generates YAML file for StatefulSet
func (r *ZookeeperClusterReconciler) yamlStatefulSet(ctx context.Context, instance *zookeeperv1beta1.ZookeeperCluster) (err error) {
	sts := zk.MakeStatefulSet(instance)

	subdir, err := yamlexporter.CreateOutputSubDir("ZookeeperCluster", sts.Labels["component"])
	return yamlexporter.GenerateOutputYAMLFile(subdir, sts.Kind, sts)
}

// yamlClientService will generates YAML file for zookeeper Client service
func (r *ZookeeperClusterReconciler) yamlClientService(ctx context.Context, instance *zookeeperv1beta1.ZookeeperCluster) (err error) {
	svc := zk.MakeClientService(instance)

	subdir, err := yamlexporter.CreateOutputSubDir("ZookeeperCluster", "client")
	if err != nil {
		return err
	}
	return yamlexporter.GenerateOutputYAMLFile(subdir, svc.Kind, svc)
}

// yamlHeadlessService will generates YAML file for zookeeper headless service
func (r *ZookeeperClusterReconciler) yamlHeadlessService(ctx context.Context, instance *zookeeperv1beta1.ZookeeperCluster) (err error) {
	svc := zk.MakeHeadlessService(instance)

	subdir, err := yamlexporter.CreateOutputSubDir("ZookeeperCluster", "headless")
	if err != nil {
		return err
	}
	return yamlexporter.GenerateOutputYAMLFile(subdir, svc.Kind, svc)
}

// yamlPodDisruptionBudget will generates YAML file for zookeeper PDB
func (r *ZookeeperClusterReconciler) yamlPodDisruptionBudget(ctx context.Context, instance *zookeeperv1beta1.ZookeeperCluster) (err error) {
	pdb := zk.MakePodDisruptionBudget(instance)

	subdir, err := yamlexporter.CreateOutputSubDir("ZookeeperCluster", "pdb")
	if err != nil {
		return err
	}
	return yamlexporter.GenerateOutputYAMLFile(subdir, pdb.Kind, pdb)
}

// yamlConfigMap will generates YAML file for Zookeeper configmap
func (r *ZookeeperClusterReconciler) yamlConfigMap(ctx context.Context, instance *zookeeperv1beta1.ZookeeperCluster) (err error) {
	cm := zk.MakeConfigMap(instance)

	subdir, err := yamlexporter.CreateOutputSubDir("ZookeeperCluster", "config")
	if err != nil {
		return err
	}
	return yamlexporter.GenerateOutputYAMLFile(subdir, cm.Kind, cm)
}

func (r *ZookeeperClusterReconciler) reconcileFinalizers(ctx context.Context, instance *zookeeperv1beta1.ZookeeperCluster) (err error) {
	ctx, span := r.Tracer.Start(ctx, "reconcileFinalizers")
	defer span.End()
	if instance.Spec.Persistence != nil && instance.Spec.Persistence.VolumeReclaimPolicy != zookeeperv1beta1.VolumeReclaimPolicyDelete {
		return nil
	}
	if instance.DeletionTimestamp.IsZero() {
		if !utils.ContainsString(instance.ObjectMeta.Finalizers, utils.ZkFinalizer) && !config.DisableFinalizer {
			instance.ObjectMeta.Finalizers = append(instance.ObjectMeta.Finalizers, utils.ZkFinalizer)
			if err = r.Client.Update(ctx, instance); err != nil {
				return err
			}
		}
		return r.cleanupOrphanPVCs(ctx, instance)
	} else {
		if utils.ContainsString(instance.ObjectMeta.Finalizers, utils.ZkFinalizer) {
			if err = r.cleanUpAllPVCs(ctx, instance); err != nil {
				return err
			}
			instance.ObjectMeta.Finalizers = utils.RemoveString(instance.ObjectMeta.Finalizers, utils.ZkFinalizer)
			if err = r.Client.Update(ctx, instance); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *ZookeeperClusterReconciler) getPVCCount(ctx context.Context, instance *zookeeperv1beta1.ZookeeperCluster) (pvcCount int, err error) {
	pvcList, err := r.getPVCList(ctx, instance)
	if err != nil {
		return -1, err
	}
	pvcCount = len(pvcList.Items)
	return pvcCount, nil
}

func (r *ZookeeperClusterReconciler) cleanupOrphanPVCs(ctx context.Context, instance *zookeeperv1beta1.ZookeeperCluster) (err error) {
	// this check should make sure we do not delete the PVCs before the STS has scaled down
	if instance.Status.ReadyReplicas == instance.Spec.Replicas {
		pvcCount, err := r.getPVCCount(ctx, instance)
		if err != nil {
			return err
		}
		r.Log.Info("cleanupOrphanPVCs", "PVC Count", pvcCount, "ReadyReplicas Count", instance.Status.ReadyReplicas)
		if pvcCount > int(instance.Spec.Replicas) {
			pvcList, err := r.getPVCList(ctx, instance)
			if err != nil {
				return err
			}
			for _, pvcItem := range pvcList.Items {
				// delete only Orphan PVCs
				if utils.IsPVCOrphan(pvcItem.Name, instance.Spec.Replicas) {
					r.deletePVC(ctx, pvcItem)
				}
			}
		}
	}
	return nil
}

func (r *ZookeeperClusterReconciler) getPVCList(ctx context.Context, instance *zookeeperv1beta1.ZookeeperCluster) (pvList corev1.PersistentVolumeClaimList, err error) {
	selector, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{
		MatchLabels: map[string]string{"app": instance.GetName(), "uid": string(instance.UID)},
	})
	pvclistOps := &client.ListOptions{
		Namespace:     instance.Namespace,
		LabelSelector: selector,
	}
	pvcList := &corev1.PersistentVolumeClaimList{}
	err = r.Client.List(ctx, pvcList, pvclistOps)
	return *pvcList, err
}

func (r *ZookeeperClusterReconciler) cleanUpAllPVCs(ctx context.Context, instance *zookeeperv1beta1.ZookeeperCluster) (err error) {
	pvcList, err := r.getPVCList(ctx, instance)
	if err != nil {
		return err
	}
	for _, pvcItem := range pvcList.Items {
		r.deletePVC(ctx, pvcItem)
	}
	return nil
}

func (r *ZookeeperClusterReconciler) deletePVC(ctx context.Context, pvcItem corev1.PersistentVolumeClaim) {
	pvcDelete := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcItem.Name,
			Namespace: pvcItem.Namespace,
		},
	}
	r.Log.Info("Deleting PVC", "With Name", pvcItem.Name)
	err := r.Client.Delete(ctx, pvcDelete)
	if err != nil {
		r.Log.Error(err, "Error deleteing PVC.", "Name", pvcDelete.Name)
	}
}

func (r *ZookeeperClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&zookeeperv1beta1.ZookeeperCluster{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.Pod{}).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		Complete(r)
}
