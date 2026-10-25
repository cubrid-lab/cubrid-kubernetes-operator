/*
Copyright 2026.

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

// Package installed runs a namespace-scoped SQL connection smoke against an
// existing Operator. It never installs or removes shared infrastructure.
package installed

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

const (
	smokePVCName        = "data-smoke-0"
	dataClaimName       = "data"
	ownerNonceKey       = "nonce"
	preallocatedLane    = "lab-preallocated-component-smoke"
	ownedWorkloadsScope = "smoke-cr-uid-and-owned-workloads"
)

type ownershipEpoch struct {
	ConfigMap string `json:"configMap"`
	UID       string `json:"uid"`
	Nonce     string `json:"nonce"`
}

type pvcIdentity struct {
	Namespace    string `json:"namespace"`
	Name         string `json:"name"`
	UID          string `json:"uid"`
	VolumeName   string `json:"volumeName"`
	StorageClass string `json:"storageClass"`
}

// namespaceGuard never obtains authority over a namespace or PVC. The external
// owner must exclude concurrent writers; reads and name-based exec are not atomic.
type namespaceGuard struct {
	runner         *Runner
	cfg            Config
	rec            *record
	cluster        *databasev1alpha1.CubridCluster
	seen           map[string]string
	managerSets    map[string]types.UID
	requireStorage bool
	cleaning       bool
	// cleanupSets holds the manager ReplicaSets of the recorded Deployment seen
	// by the current cleanup inventory.
	cleanupSets map[string]types.UID
	// managerChanged records that cleanup saw another manager incarnation.
	managerChanged bool
}

func (g *namespaceGuard) epoch(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ns := &corev1.Namespace{}
	if err := g.runner.Client.Get(ctx, client.ObjectKey{Name: g.cfg.Namespace}, ns); err != nil {
		return err
	}
	if string(ns.UID) != g.cfg.NamespaceUID || !ns.DeletionTimestamp.IsZero() {
		return errors.New("preallocated namespace epoch changed or is terminating; resources preserved")
	}
	marker := &corev1.ConfigMap{}
	if err := g.runner.Client.Get(ctx, client.ObjectKey{Namespace: g.cfg.Namespace, Name: g.cfg.OwnerConfigMap},
		marker); err != nil {
		return err
	}
	if string(marker.UID) != g.cfg.OwnerUID || marker.Immutable == nil || !*marker.Immutable ||
		marker.Data[ownerNonceKey] != g.cfg.OwnerNonce || !marker.DeletionTimestamp.IsZero() {
		return errors.New("external immutable owner epoch changed; resources preserved")
	}
	return nil
}

func (g *namespaceGuard) remember(kind string, obj client.Object) error {
	key := kind + "/" + obj.GetName()
	uid := string(obj.GetUID())
	if uid == "" {
		return fmt.Errorf("%s has no observed UID", key)
	}
	if previous, ok := g.seen[key]; ok && previous != uid {
		return fmt.Errorf("%s was replaced; resources preserved", key)
	}
	g.seen[key] = uid
	return nil
}

func soleController(obj metav1.Object, kind, name string, uid types.UID) bool {
	apiVersion := "apps/v1"
	if kind == "CubridCluster" {
		apiVersion = "database.cubrid.io/v1alpha1"
	}
	refs := obj.GetOwnerReferences()
	return len(refs) == 1 && refs[0].Controller != nil && *refs[0].Controller &&
		refs[0].APIVersion == apiVersion && refs[0].Kind == kind && refs[0].Name == name && refs[0].UID == uid && uid != ""
}

// brokerChain reports whether obj belongs to the created cluster's Broker
// tier: the <cluster>-broker-rw or <cluster>-broker-ro Deployment the cluster
// solely controls, a ReplicaSet of such a Deployment, or a Pod of such a
// ReplicaSet. Parents are matched by the UIDs pinned when they were observed,
// so a child outliving its parent during GC is still identified and a
// replacement never inherits the identity.
func (g *namespaceGuard) brokerChain(obj client.Object) bool {
	if g.cluster == nil || len(obj.GetOwnerReferences()) != 1 {
		return false
	}
	parent := obj.GetOwnerReferences()[0].Name
	switch obj.(type) {
	case *appsv1.Deployment:
		return (obj.GetName() == clusterName+"-broker-rw" || obj.GetName() == clusterName+"-broker-ro") &&
			soleController(obj, "CubridCluster", clusterName, g.cluster.UID)
	case *appsv1.ReplicaSet:
		return soleController(obj, "Deployment", parent, types.UID(g.seen["Deployment/"+parent]))
	case *corev1.Pod:
		return soleController(obj, "ReplicaSet", parent, types.UID(g.seen["ReplicaSet/"+parent]))
	}
	return false
}

func (g *namespaceGuard) check(ctx context.Context) error {
	if err := g.epoch(ctx); err != nil {
		return err
	}
	if err := g.checkManager(ctx); err != nil {
		return err
	}

	// Pods are read before their controllers: a controller creates its parent
	// before any child, so every Pod read here has its parent in the later
	// workload reads, even while the Operator or a rollout creates new ones.
	pods := &corev1.PodList{}
	if err := g.runner.Client.List(ctx, pods, client.InNamespace(g.cfg.Namespace)); err != nil {
		return err
	}
	for _, check := range []func(context.Context) error{g.checkClusters, g.checkManagerWorkloads,
		g.checkScheduledWriters} {
		if err := check(ctx); err != nil {
			return err
		}
	}
	databaseSet, err := g.checkStatefulSets(ctx)
	if err != nil {
		return err
	}
	if err := g.checkPods(pods); err != nil {
		return err
	}
	if err := g.checkClaims(ctx, databaseSet); err != nil {
		return err
	}

	return g.epoch(ctx)
}

// checkManager requires the manager the run recorded. During cleanup it requires
// only the recorded Deployment, still declaring the candidate image: deleting
// the created CR by its UID does not depend on which incarnation of the
// manager runs, and a manager that restarted during the run must not leave the
// smoke cluster behind (#507). A changed incarnation still fails the run.
func (g *namespaceGuard) checkManager(ctx context.Context) error {
	if !g.cleaning {
		current, err := g.runner.observeManager(ctx, g.cfg)
		if err != nil {
			return err
		}
		if current.DeploymentUID != g.rec.Operator.DeploymentUID || !slices.Equal(current.Pods, g.rec.Operator.Pods) {
			return errors.New("candidate manager identity changed")
		}
		return nil
	}
	deployment := &appsv1.Deployment{}
	key := client.ObjectKey{Namespace: g.cfg.OperatorNamespace, Name: g.cfg.OperatorDeployment}
	if err := g.runner.Client.Get(ctx, key, deployment); err != nil {
		return fmt.Errorf("observe installed manager: %w", err)
	}
	if string(deployment.UID) != g.rec.Operator.DeploymentUID || g.rec.Operator.DeploymentUID == "" ||
		!deployment.DeletionTimestamp.IsZero() {
		return errors.New("candidate manager Deployment was replaced; resources preserved")
	}
	if managerImage(deployment.Spec.Template.Spec) != g.cfg.OperatorImage {
		return errors.New("candidate manager Deployment no longer declares the candidate image; resources preserved")
	}
	if current, err := g.runner.observeManager(ctx, g.cfg); err != nil ||
		!slices.Equal(current.Pods, g.rec.Operator.Pods) {
		g.managerChanged = true
	}
	return nil
}

// managerImage returns the image of the manager container of spec.
func managerImage(spec corev1.PodSpec) string {
	for _, c := range spec.Containers {
		if c.Name == managerContainer {
			return c.Image
		}
	}
	return ""
}

// managerVolume reports whether volume is one a manager Pod may have during
// cleanup: no PVC, hostPath, CSI, ephemeral or other storage that could reach
// the retained data.
func managerVolume(volume corev1.Volume) bool {
	s := volume.VolumeSource
	return s.Projected != nil || s.ConfigMap != nil || s.Secret != nil || s.EmptyDir != nil || s.DownwardAPI != nil
}

// managerSet reports whether a ReplicaSet of the given name and UID is one the
// manager may run Pods from: one of those seen at the first inventory, or,
// during cleanup, any ReplicaSet the recorded Deployment solely controls.
func (g *namespaceGuard) managerSet(name string, uid types.UID) bool {
	if uid == "" {
		return false
	}
	return g.managerSets[name] == uid || (g.cleaning && g.cleanupSets[name] == uid)
}

func (g *namespaceGuard) checkClusters(ctx context.Context) error {
	c, ns := g.runner.Client, client.InNamespace(g.cfg.Namespace)
	clusters, backups := &databasev1alpha1.CubridClusterList{}, &databasev1alpha1.CubridBackupList{}
	if err := c.List(ctx, clusters, ns); err != nil {
		return err
	}
	if err := c.List(ctx, backups, ns); err != nil {
		return err
	}
	if g.cluster != nil && !g.cleaning && len(clusters.Items) != 1 {
		return errors.New("created smoke cluster disappeared before work completed")
	}
	if len(backups.Items) != 0 {
		return errors.New("preallocated namespace contains a backup; resources preserved")
	}
	for i := range clusters.Items {
		obj := &clusters.Items[i]
		if g.cluster == nil || obj.Name != g.cluster.Name || obj.UID != g.cluster.UID {
			return errors.New("preallocated namespace contains a foreign or replacement cluster; resources preserved")
		}
	}

	return nil
}

func (g *namespaceGuard) checkManagerWorkloads(ctx context.Context) error {
	c, ns := g.runner.Client, client.InNamespace(g.cfg.Namespace)
	deployments, sets := &appsv1.DeploymentList{}, &appsv1.ReplicaSetList{}
	if err := c.List(ctx, deployments, ns); err != nil {
		return err
	}
	if err := c.List(ctx, sets, ns); err != nil {
		return err
	}
	for i := range deployments.Items {
		obj := &deployments.Items[i]
		if g.brokerChain(obj) {
			if err := g.remember("Deployment", obj); err != nil {
				return err
			}
			continue
		}
		if g.cfg.Namespace != g.cfg.OperatorNamespace || obj.Name != g.cfg.OperatorDeployment ||
			string(obj.UID) != g.rec.Operator.DeploymentUID || !obj.DeletionTimestamp.IsZero() {
			return errors.New("unidentified Deployment in preallocated namespace")
		}
	}
	if g.managerSets == nil {
		g.managerSets = map[string]types.UID{}
		for _, obj := range sets.Items {
			if g.brokerChain(&obj) {
				continue
			}
			if g.cfg.Namespace != g.cfg.OperatorNamespace || !soleController(&obj, "Deployment",
				g.cfg.OperatorDeployment, types.UID(g.rec.Operator.DeploymentUID)) || obj.UID == "" {
				return errors.New("unidentified bootstrap ReplicaSet")
			}
			g.managerSets[obj.Name] = obj.UID
		}
	}
	g.cleanupSets = map[string]types.UID{}
	for i := range sets.Items {
		obj := &sets.Items[i]
		if g.brokerChain(obj) {
			if err := g.remember("ReplicaSet", obj); err != nil {
				return err
			}
			continue
		}
		owned := g.cfg.Namespace == g.cfg.OperatorNamespace && soleController(obj, "Deployment",
			g.cfg.OperatorDeployment, types.UID(g.rec.Operator.DeploymentUID))
		if owned && g.cleaning {
			g.cleanupSets[obj.Name] = obj.UID
		}
		if !owned || !g.managerSet(obj.Name, obj.UID) {
			return errors.New("candidate ReplicaSet was replaced or is unidentified")
		}
	}

	return nil
}

func (g *namespaceGuard) checkScheduledWriters(ctx context.Context) error {
	c, ns := g.runner.Client, client.InNamespace(g.cfg.Namespace)
	jobs, cronjobs := &batchv1.JobList{}, &batchv1.CronJobList{}
	if err := c.List(ctx, jobs, ns); err != nil {
		return err
	}
	if err := c.List(ctx, cronjobs, ns); err != nil {
		return err
	}
	if len(jobs.Items)+len(cronjobs.Items) != 0 {
		return errors.New("unidentified Job/CronJob could start another writer")
	}

	// Dormant built-in controllers can create a future writer without a Pod now.
	daemonSets, replicationControllers := &appsv1.DaemonSetList{}, &corev1.ReplicationControllerList{}
	if err := c.List(ctx, daemonSets, ns); err != nil {
		return err
	}
	if err := c.List(ctx, replicationControllers, ns); err != nil {
		return err
	}
	if len(daemonSets.Items)+len(replicationControllers.Items) != 0 {
		return errors.New("unidentified DaemonSet/ReplicationController could start another writer")
	}

	return nil
}

func (g *namespaceGuard) checkStatefulSets(ctx context.Context) (*appsv1.StatefulSet, error) {
	c, ns := g.runner.Client, client.InNamespace(g.cfg.Namespace)
	statefulSets := &appsv1.StatefulSetList{}
	if err := c.List(ctx, statefulSets, ns); err != nil {
		return nil, err
	}
	var databaseSet *appsv1.StatefulSet
	for i := range statefulSets.Items {
		obj := &statefulSets.Items[i]
		if g.cluster == nil || obj.Name != clusterName || !soleController(obj, "CubridCluster", clusterName, g.cluster.UID) {
			return nil, errors.New("foreign StatefulSet in preallocated namespace; resources preserved")
		}
		if err := g.remember("StatefulSet", obj); err != nil {
			return nil, err
		}
		databaseSet = obj
	}

	return databaseSet, nil
}

func (g *namespaceGuard) checkPods(pods *corev1.PodList) error {
	for i := range pods.Items {
		obj := &pods.Items[i]
		manager, err := g.cleanupManagerPod(obj)
		if err != nil {
			return err
		}
		if manager {
			continue
		}
		if g.cfg.Namespace == g.cfg.OperatorNamespace {
			for _, known := range g.rec.Operator.Pods {
				if obj.Name == known.Pod && string(obj.UID) == known.UID && len(obj.OwnerReferences) == 1 &&
					soleController(obj, "ReplicaSet", obj.OwnerReferences[0].Name, g.managerSets[obj.OwnerReferences[0].Name]) {
					current, err := podIdentity(obj, "manager", g.cfg.OperatorImage)
					manager = err == nil && current == known
				}
			}
		}
		if manager {
			for _, volume := range obj.Spec.Volumes {
				if volume.PersistentVolumeClaim != nil {
					return errors.New("candidate manager Pod could mount retained data; writer ambiguity")
				}
			}
			continue
		}
		if g.brokerChain(obj) {
			for _, volume := range obj.Spec.Volumes {
				if !brokerVolume(volume) {
					return errors.New("broker Pod mounts a volume the Operator does not give it; writer ambiguity")
				}
			}
			if err := g.remember("Pod", obj); err != nil {
				return err
			}
			continue
		}
		// A terminating DB Pod may outlive its StatefulSet during GC. Its pinned
		// parent and own UIDs still identify it; a replacement never inherits them.
		parent := types.UID(g.seen["StatefulSet/"+clusterName])
		if g.cluster == nil || obj.Name != clusterName+"-0" || !soleController(obj, "StatefulSet", clusterName, parent) {
			return errors.New("unidentified or replacement Pod; stopped writers cannot be proven")
		}
		if err := g.remember("Pod", obj); err != nil {
			return err
		}
		claims := 0
		for _, volume := range obj.Spec.Volumes {
			if volume.PersistentVolumeClaim != nil {
				if volume.PersistentVolumeClaim.ClaimName != smokePVCName {
					return errors.New("database Pod has an unidentified PVC mount")
				}
				claims++
			}
		}
		if claims != 1 {
			return errors.New("database Pod does not mount the deterministic smoke PVC")
		}
	}

	return nil
}

// cleanupManagerPod reports whether, during cleanup, obj is a Pod of a
// ReplicaSet of the recorded manager Deployment in the smoke's namespace,
// ready or not. Such a Pod must run the candidate image and have only manager
// volumes.
func (g *namespaceGuard) cleanupManagerPod(obj *corev1.Pod) (bool, error) {
	if !g.cleaning || g.cfg.Namespace != g.cfg.OperatorNamespace || len(obj.OwnerReferences) != 1 ||
		!soleController(obj, "ReplicaSet", obj.OwnerReferences[0].Name, g.cleanupSets[obj.OwnerReferences[0].Name]) {
		return false, nil
	}
	if managerImage(obj.Spec) != g.cfg.OperatorImage {
		return false, errors.New("manager Pod does not run the candidate image; writer ambiguity")
	}
	if slices.ContainsFunc(obj.Spec.Volumes, func(v corev1.Volume) bool { return !managerVolume(v) }) {
		return false, errors.New("candidate manager Pod could mount retained data; writer ambiguity")
	}
	return true, nil
}

// brokerVolume reports whether volume is one a Broker Pod has on a real
// cluster: the Operator's Broker ConfigMap, or the projected service-account
// token volume admission injects (token, kube-root-ca.crt, namespace).
func brokerVolume(volume corev1.Volume) bool {
	source := volume.VolumeSource
	switch {
	case source.ConfigMap != nil:
		return source == corev1.VolumeSource{ConfigMap: source.ConfigMap} &&
			source.ConfigMap.Name == clusterName+"-broker-config"
	case source.Projected != nil:
		if source != (corev1.VolumeSource{Projected: source.Projected}) {
			return false
		}
		for _, p := range source.Projected.Sources {
			if p != (corev1.VolumeProjection{ServiceAccountToken: p.ServiceAccountToken}) &&
				p != (corev1.VolumeProjection{ConfigMap: p.ConfigMap}) &&
				p != (corev1.VolumeProjection{DownwardAPI: p.DownwardAPI}) {
				return false
			}
		}
		return true
	}
	return false
}

func (g *namespaceGuard) checkClaims(ctx context.Context, databaseSet *appsv1.StatefulSet) error {
	c, ns := g.runner.Client, client.InNamespace(g.cfg.Namespace)
	claims := &corev1.PersistentVolumeClaimList{}
	if err := c.List(ctx, claims, ns); err != nil {
		return err
	}
	for i := range claims.Items {
		obj := &claims.Items[i]
		if g.cluster == nil || obj.Name != smokePVCName || g.seen["StatefulSet/"+clusterName] == "" ||
			len(obj.OwnerReferences) != 0 || obj.Spec.StorageClassName == nil ||
			*obj.Spec.StorageClassName != g.cfg.StorageClass ||
			obj.Spec.Resources.Requests.Storage().String() != "2Gi" || !obj.DeletionTimestamp.IsZero() {
			return errors.New("foreign or ambiguous PVC; resources preserved")
		}
		if databaseSet != nil && (len(databaseSet.Spec.VolumeClaimTemplates) != 1 ||
			databaseSet.Spec.VolumeClaimTemplates[0].Name != dataClaimName) {
			return errors.New("smoke StatefulSet has an unexpected claim template")
		}
		if err := g.remember("PersistentVolumeClaim", obj); err != nil {
			return err
		}
		observed := pvcIdentity{Namespace: g.cfg.Namespace, Name: obj.Name,
			UID: string(obj.UID), VolumeName: obj.Spec.VolumeName,
			StorageClass: *obj.Spec.StorageClassName}
		previous := slices.IndexFunc(g.rec.RetainedPVCs, func(p pvcIdentity) bool { return p.Name == obj.Name })
		if previous < 0 {
			g.rec.RetainedPVCs = append(g.rec.RetainedPVCs, observed)
		} else {
			old := g.rec.RetainedPVCs[previous]
			if old.VolumeName != "" && old.VolumeName != observed.VolumeName {
				return errors.New("recorded PVC backing volume changed")
			}
			g.rec.RetainedPVCs[previous] = observed
		}
	}
	if g.requireStorage && len(claims.Items) != 1 {
		return errors.New("the recorded retained smoke PVC is missing")
	}
	return nil
}

func (g *namespaceGuard) cleanup(ctx context.Context, poll time.Duration) error {
	g.cleaning = true
	defer func() { g.rec.ManagerChangedAtCleanup = g.managerChanged }()
	if err := g.check(ctx); err != nil {
		return err
	}
	// Only a successful create with its returned UID grants this authority.
	if g.cluster == nil || g.cluster.UID == "" {
		return errors.New("no created CR UID cleanup authority")
	}
	uid := g.cluster.UID
	opts := &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}
	if err := g.runner.Client.Delete(ctx, g.cluster, opts); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return pollUntil(ctx, poll, func() (bool, error) {
		if err := g.check(ctx); err != nil {
			return false, err
		}
		current := &databasev1alpha1.CubridCluster{}
		if err := g.runner.Client.Get(ctx, client.ObjectKeyFromObject(g.cluster), current); err == nil {
			return false, nil
		} else if !apierrors.IsNotFound(err) {
			return false, err
		}
		for _, obj := range []client.Object{&appsv1.StatefulSet{}, &corev1.Pod{}} {
			name := clusterName
			if _, ok := obj.(*corev1.Pod); ok {
				name += "-0"
			}
			err := g.runner.Client.Get(ctx, client.ObjectKey{Namespace: g.cfg.Namespace, Name: name}, obj)
			if err == nil {
				return false, nil
			}
			if !apierrors.IsNotFound(err) {
				return false, err
			}
		}
		if remaining, err := g.brokerRemains(ctx); err != nil || remaining {
			return false, err
		}
		return true, g.epoch(ctx)
	})
}

// brokerRemains reports whether any object of the created cluster's Broker
// tier is still present.
func (g *namespaceGuard) brokerRemains(ctx context.Context) (bool, error) {
	ns := client.InNamespace(g.cfg.Namespace)
	deployments, sets, pods := &appsv1.DeploymentList{}, &appsv1.ReplicaSetList{}, &corev1.PodList{}
	for _, list := range []client.ObjectList{deployments, sets, pods} {
		if err := g.runner.Client.List(ctx, list, ns); err != nil {
			return false, err
		}
	}
	for i := range deployments.Items {
		if g.brokerChain(&deployments.Items[i]) {
			return true, nil
		}
	}
	for i := range sets.Items {
		if g.brokerChain(&sets.Items[i]) {
			return true, nil
		}
	}
	for i := range pods.Items {
		if g.brokerChain(&pods.Items[i]) {
			return true, nil
		}
	}
	return false, nil
}
