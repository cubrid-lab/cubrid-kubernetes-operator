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

package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/prometheus/client_golang/prometheus"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/metrics"
)

const (
	// cubridBrokerPort is the default CUBRID broker/CAS port.
	cubridBrokerPort = 33000
	// cubridServerPort is the default CUBRID master/server port.
	cubridServerPort = 1523
	// instanceManagerPort is the Instance Manager HTTP API port (ADR-0003).
	instanceManagerPort = 9090

	// Pod runtime contract with the Instance Manager image (ADR-0003, #97/#98).
	//
	// DefaultInstanceManagerImage is used when neither spec.image nor the
	// operator's --instance-manager-image setting names an image.
	DefaultInstanceManagerImage = "ghcr.io/cubrid-lab/cubrid-kubernetes-operator/instance-manager:dev"
	// dataMountPath is where the data PVC is mounted; CUBRID_DATABASES points
	// below it so databases live on the PVC, not in the image's own volume.
	dataMountPath = "/var/lib/cubrid"
	// operationsDir holds the Instance Manager's durable operation records.
	operationsDir = dataMountPath + "/operations"
	// cubridUID is the cubrid user and group of the official CUBRID image.
	cubridUID int64 = 1000
	// imTokenKey is the key of the Instance Manager token in <cluster>-im-token.
	imTokenKey = "token"
	// imPreviousTokenKey holds the token replaced by a rotation, which Pods
	// started before the rotation still hold, until every member was
	// started after it.
	imPreviousTokenKey = "previousToken"
	// imTokenRotatedAtAnnotation records on <cluster>-im-token when the
	// current token was written. A Secret without it was written by an
	// operator that copied one token into every cluster, or by the first one
	// with per-cluster tokens, and is rotated once.
	imTokenRotatedAtAnnotation = "database.cubrid.io/im-token-rotated-at"
	// rotateIMTokenAnnotation on a CubridCluster asks for a new token. Each
	// new value is one rotation; the value handled last is recorded on the
	// Secret under the same name.
	rotateIMTokenAnnotation = "database.cubrid.io/rotate-im-token"

	// Condition types (ADR-0005/0006).
	conditionReady       = "Ready"
	conditionProgressing = "Progressing"
	conditionHAReady     = "HAReady"

	appName = "cubrid"
)

// CubridClusterReconciler reconciles a CubridCluster object.
type CubridClusterReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	// Prober is nil-safe: nil skips HA role discovery (HAReady=Unknown).
	Prober RoleProber
	// TokenChecker confirms that every member accepts the current Instance
	// Manager token before the previous one is dropped. Nil keeps the
	// previous token: no member is then known to hold the current one.
	TokenChecker TokenChecker
	// Restore is nil-safe: nil skips recovery-bootstrap orchestration
	// (BootstrapReady=False/RestoreClientNotConfigured).
	Restore RestoreClient
	// HABootstrap is nil-safe: nil leaves the first database of an HA cluster
	// uncreated.
	HABootstrap HABootstrapClient
	// Backup is nil-safe: with Restore it seeds the other members of an HA
	// cluster from the first one; nil leaves them unseeded.
	Backup BackupClient
	// AutomaticReplacement lets the operator delete outdated slave Pods for a
	// rolling update. It is off: the conditions that would make a replacement
	// safe are not complete (ADR-0009, #96). The planning runs either way.
	AutomaticReplacement bool
	// DefaultImage is the DB Pod image when spec.image is not set; empty falls
	// back to DefaultInstanceManagerImage.
	DefaultImage string
	// Clock judges observation freshness; nil means time.Now.
	Clock func() time.Time
	// OperatorNS is the namespace of the operator's Pods, the only Pods the
	// NetworkPolicy of the DB Pods admits to the Instance Manager port. Empty
	// admits none.
	OperatorNS string
}

func (r *CubridClusterReconciler) now() time.Time {
	if r.Clock != nil {
		return r.Clock()
	}
	return time.Now()
}

// +kubebuilder:rbac:groups=database.cubrid.io,resources=cubridclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=database.cubrid.io,resources=cubridclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=database.cubrid.io,resources=cubridclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete

// Reconcile drives the CubridCluster toward its desired state: a headless
// governing Service and a StatefulSet with a data PVC template; HA role
// discovery + safety-first status (ADR-0005); the broker tier (ADR-0002);
// recovery-bootstrap orchestration (ADR-0008); and the engine-version guard +
// slaves-first rolling update (ADR-0009).
func (r *CubridClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var cluster databasev1alpha1.CubridCluster
	if err := r.Get(ctx, req.NamespacedName, &cluster); err != nil {
		// Ignore not-found: the object was deleted; owned resources are GC'd
		// via owner references.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// What changed in the status during this reconcile is logged once, when
	// it ends, whichever way it ends (docs/observability.md).
	before := cluster.Status.DeepCopy()
	defer func() { r.reportTransitions(ctx, &cluster, before) }()

	// Reconcile the governing headless Service (ADR-0004: stable per-pod DNS).
	if err := r.reconcileHeadlessService(ctx, &cluster); err != nil {
		log.Error(err, "Failed to reconcile headless Service")
		return r.failed(ctx, &cluster, "ServiceReconcileFailed", err)
	}

	// One alias Service per member, so its short HA host name resolves.
	if err := r.reconcileMemberServices(ctx, &cluster); err != nil {
		log.Error(err, "Failed to reconcile member Services")
		return r.failed(ctx, &cluster, "MemberServiceReconcileFailed", err)
	}

	// The DB Pods authenticate the operator with the cluster's own token
	// (ADR-0003), which exists before the first Pod does.
	if err := r.reconcileIMTokenSecret(ctx, &cluster); err != nil {
		log.Error(err, "Failed to reconcile Instance Manager token Secret")
		return r.failed(ctx, &cluster, "SecretReconcileFailed", err)
	}

	// The HA member list every member reads (ADR-0004); none when standalone.
	if err := r.reconcileHAConfig(ctx, &cluster); err != nil {
		log.Error(err, "Failed to reconcile HA ConfigMap")
		return r.failed(ctx, &cluster, "HAConfigReconcileFailed", err)
	}

	// The ingress NetworkPolicies exist before the first DB Pod does.
	if err := r.reconcileNetworkPolicies(ctx, &cluster); err != nil {
		log.Error(err, "Failed to reconcile NetworkPolicies")
		return r.failed(ctx, &cluster, "NetworkPolicyReconcileFailed", err)
	}

	// Reconcile the StatefulSet (DB instances + per-ordinal data PVC).
	sts, err := r.reconcileStatefulSet(ctx, &cluster)
	if err != nil {
		log.Error(err, "Failed to reconcile StatefulSet")
		return r.failed(ctx, &cluster, "StatefulSetReconcileFailed", err)
	}

	return r.updateStatus(ctx, &cluster, sts)
}

// instancesServiceName is the governing headless Service (<cluster>-instances).
func instancesServiceName(name string) string { return name + "-instances" }

// labelsFor returns the standard selector labels for a cluster's DB instances.
func labelsFor(cluster *databasev1alpha1.CubridCluster) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       appName,
		"app.kubernetes.io/instance":   cluster.Name,
		"app.kubernetes.io/component":  "database",
		"app.kubernetes.io/managed-by": "cubrid-kubernetes-operator",
	}
}

func (r *CubridClusterReconciler) reconcileHeadlessService(ctx context.Context, cluster *databasev1alpha1.CubridCluster) error {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instancesServiceName(cluster.Name),
			Namespace: cluster.Namespace,
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = labelsFor(cluster)
		svc.Spec.ClusterIP = corev1.ClusterIPNone
		// publishNotReadyAddresses keeps peer DNS resolvable during startup/
		// failover when a pod is briefly NotReady (ADR-0004).
		svc.Spec.PublishNotReadyAddresses = true
		svc.Spec.Selector = labelsFor(cluster)
		svc.Spec.Ports = []corev1.ServicePort{
			{Name: portNameCubrid, Protocol: corev1.ProtocolTCP, Port: cubridServerPort, TargetPort: intOrString(cubridServerPort)},
			{Name: "broker", Protocol: corev1.ProtocolTCP, Port: cubridBrokerPort, TargetPort: intOrString(cubridBrokerPort)},
			{Name: portNameManager, Protocol: corev1.ProtocolTCP, Port: instanceManagerPort, TargetPort: intOrString(instanceManagerPort)},
		}
		return controllerutil.SetControllerReference(cluster, svc, r.Scheme)
	})
	return err
}

// instanceManagerBinary is the Instance Manager in the image
// (build/instance-manager/Dockerfile).
const instanceManagerBinary = "/usr/local/bin/instance-manager"

func imTokenSecretName(cluster string) string { return cluster + "-im-token" }

// reconcileIMTokenSecret gives the cluster a random token of its own in
// <cluster>-im-token, which its DB Pods serve with and the operator calls them
// with (ClusterTokens). A Pod's container reads the token when it starts, so a
// running Pod keeps the token it started with. A missing or empty token is
// generated anew. Otherwise a token is only replaced with an overlap: when the
// Secret has no rotation time, or when rotateIMTokenAnnotation asks for a new
// token, the current token is kept as the previous one, which the operator
// still calls members with, and a new one is generated. The previous token is
// dropped once every member was started after the rotation and accepts the
// current token (membersHoldToken). No rotation starts while a previous token
// is kept: it would drop the token running Pods hold.
func (r *CubridClusterReconciler) reconcileIMTokenSecret(ctx context.Context, cluster *databasev1alpha1.CubridCluster) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: imTokenSecretName(cluster.Name), Namespace: cluster.Namespace},
	}
	request := cluster.Annotations[rotateIMTokenAnnotation]
	var generated, rotated, overlapEnded bool
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		secret.Labels = labelsFor(cluster)
		secret.Type = corev1.SecretTypeOpaque
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		if secret.Annotations == nil {
			secret.Annotations = map[string]string{}
		}
		rotatedAt := secret.Annotations[imTokenRotatedAtAnnotation]
		switch {
		case len(secret.Data[imTokenKey]) == 0:
			generated = true
		case len(secret.Data[imPreviousTokenKey]) > 0:
			since, err := time.Parse(time.RFC3339, rotatedAt)
			if err != nil {
				// Without a readable rotation time no member is known to hold
				// the current token; members started from now on do.
				secret.Annotations[imTokenRotatedAtAnnotation] = r.now().UTC().Format(time.RFC3339)
				break
			}
			ended, err := r.membersHoldToken(ctx, cluster, since, string(secret.Data[imTokenKey]))
			if err != nil {
				return err
			}
			if ended {
				delete(secret.Data, imPreviousTokenKey)
				overlapEnded = true
			}
		case rotatedAt == "", request != "" && request != secret.Annotations[rotateIMTokenAnnotation]:
			rotated = true
		}
		if generated || rotated {
			token, err := newIMToken()
			if err != nil {
				return err
			}
			if rotated {
				secret.Data[imPreviousTokenKey] = secret.Data[imTokenKey]
			}
			secret.Data[imTokenKey] = []byte(token)
			if request != "" {
				secret.Annotations[rotateIMTokenAnnotation] = request
			}
		}
		return controllerutil.SetControllerReference(cluster, secret, r.Scheme)
	})
	if err != nil {
		return err
	}
	if generated || rotated {
		// The rotation time is taken once the Secret holds the new token, so
		// that no container started before the write counts as started after.
		if secret.Annotations == nil {
			secret.Annotations = map[string]string{}
		}
		secret.Annotations[imTokenRotatedAtAnnotation] = r.now().UTC().Format(time.RFC3339)
		if err := r.Update(ctx, secret); err != nil {
			return err
		}
	}
	switch {
	case rotated:
		r.event(cluster, corev1.EventTypeNormal, "InstanceManagerTokenRotated",
			fmt.Sprintf("generated a new Instance Manager token in Secret %s and kept the previous one, which running Pods hold, for calls to them; replace the cluster's Pods one at a time, and the previous token is dropped once every member was started with the new one",
				secret.Name))
		return nil
	case overlapEnded:
		r.event(cluster, corev1.EventTypeNormal, "InstanceManagerTokenOverlapEnded",
			fmt.Sprintf("every member was started after the Instance Manager token was rotated and accepts the new one; dropped the previous token from Secret %s",
				secret.Name))
		return nil
	case !generated:
		if len(secret.Data[imPreviousTokenKey]) > 0 && request != "" && request != secret.Annotations[rotateIMTokenAnnotation] {
			r.event(cluster, corev1.EventTypeNormal, "InstanceManagerTokenRotationDeferred",
				fmt.Sprintf("the requested Instance Manager token rotation waits until every member holds the current token of Secret %s",
					secret.Name))
		}
		return nil
	}
	// A new token under an existing StatefulSet: its running Pods still hold
	// the old one and refuse the operator until each of them is replaced.
	err = r.Get(ctx, client.ObjectKey{Name: cluster.Name, Namespace: cluster.Namespace}, &appsv1.StatefulSet{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	r.event(cluster, corev1.EventTypeWarning, "InstanceManagerTokenRegenerated",
		fmt.Sprintf("generated a new Instance Manager token in Secret %s; running Pods keep the old one and refuse the operator until they are replaced",
			secret.Name))
	return nil
}

// membersHoldToken reports whether every member of the cluster holds the
// token. A member that is missing, not running, or running since before the
// rotation may hold the previous token and is not asked. Every other member
// must accept the token itself (TokenChecker): the start time alone could
// mislead when the node's clock is ahead of the operator's or the kubelet
// read a stale Secret. A member that refuses the token or cannot be asked
// keeps the previous one in use.
func (r *CubridClusterReconciler) membersHoldToken(ctx context.Context, cluster *databasev1alpha1.CubridCluster, since time.Time, token string) (bool, error) {
	if r.TokenChecker == nil {
		return false, nil
	}
	names := memberNames(cluster, cluster.Spec.Topology.PromotableMembers)
	for _, name := range names {
		var pod corev1.Pod
		if err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: cluster.Namespace}, &pod); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		started := false
		for _, s := range pod.Status.ContainerStatuses {
			if s.Name == appName && s.State.Running != nil {
				started = s.State.Running.StartedAt.After(since)
			}
		}
		if !started {
			return false, nil
		}
	}
	for _, name := range names {
		accepted, err := r.TokenChecker.AcceptsToken(ctx, name, cluster.Namespace, token)
		if err != nil {
			logf.FromContext(ctx).V(1).Info("Could not ask member about the current Instance Manager token; previous token kept",
				"pod", name, "reason", err.Error())
			return false, nil
		}
		if !accepted {
			return false, nil
		}
	}
	return true, nil
}

// newIMToken returns 256 random bits, hex-encoded.
func newIMToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate Instance Manager token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func (r *CubridClusterReconciler) reconcileStatefulSet(ctx context.Context, cluster *databasev1alpha1.CubridCluster) (*appsv1.StatefulSet, error) {
	replicas := cluster.Spec.Topology.PromotableMembers
	labels := labelsFor(cluster)

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: cluster.Name, Namespace: cluster.Namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, sts, func() error {
		sts.Labels = labels
		// Immutable fields (selector, volumeClaimTemplates, serviceName) are only
		// set on creation; CreateOrUpdate skips them on update by leaving the
		// existing values in place for a freshly-fetched object.
		if sts.CreationTimestamp.IsZero() {
			sts.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
			sts.Spec.ServiceName = instancesServiceName(cluster.Name)
			sts.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{r.dataPVCTemplate(cluster)}
			// Parallel: no member waits for a lower ordinal to be Ready, so a
			// full restart never starts member 0 alone and no ordinal is
			// favoured (ADR-0001, "Pod management policy").
			sts.Spec.PodManagementPolicy = appsv1.ParallelPodManagement
		}
		sts.Spec.Replicas = &replicas
		// OnDelete: the operator owns pod replacement sequencing (ADR-0003/0009).
		sts.Spec.UpdateStrategy = appsv1.StatefulSetUpdateStrategy{Type: appsv1.OnDeleteStatefulSetStrategyType}
		sts.Spec.PersistentVolumeClaimRetentionPolicy = pvcRetentionPolicy(cluster)
		if err := setPodTemplate(sts, &sts.Spec.Template, corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: labels},
			Spec:       r.podSpec(cluster, r.imageToRun(cluster, sts)),
		}); err != nil {
			return err
		}
		return controllerutil.SetControllerReference(cluster, sts, r.Scheme)
	})
	if err != nil {
		return nil, err
	}
	return sts, nil
}

// pvcRetentionPolicy maps spec.storage.retentionPolicy to StatefulSet PVC
// retention. WhenScaled is ALWAYS Retain: ADR-0006 forbids implicit PVC
// deletion on scale-down, even when retentionPolicy is Delete.
func pvcRetentionPolicy(cluster *databasev1alpha1.CubridCluster) *appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy {
	whenDeleted := appsv1.RetainPersistentVolumeClaimRetentionPolicyType
	if cluster.Spec.Storage.RetentionPolicy == "Delete" {
		whenDeleted = appsv1.DeletePersistentVolumeClaimRetentionPolicyType
	}
	return &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
		WhenDeleted: whenDeleted,
		WhenScaled:  appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
	}
}

func (r *CubridClusterReconciler) dataPVCTemplate(cluster *databasev1alpha1.CubridCluster) corev1.PersistentVolumeClaim {
	return corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data"},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: cluster.Spec.Storage.Data.StorageClassName,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: cluster.Spec.Storage.Data.Size},
			},
		},
	}
}

// instanceImage is the image of the DB Pods and of the Broker Pods: spec.image
// if set, else the operator's default Instance Manager image (ADR-0003).
func (r *CubridClusterReconciler) instanceImage(cluster *databasev1alpha1.CubridCluster) string {
	image := r.DefaultImage
	if image == "" {
		image = DefaultInstanceManagerImage
	}
	if cluster.Spec.Image != nil && cluster.Spec.Image.Repository != "" {
		image = cluster.Spec.Image.Repository
		if cluster.Spec.Image.Tag != "" {
			image = fmt.Sprintf("%s:%s", cluster.Spec.Image.Repository, cluster.Spec.Image.Tag)
		}
	}
	return image
}

// runningImage is the image in the StatefulSet's Pod template, "" before the
// StatefulSet exists.
func runningImage(sts *appsv1.StatefulSet) string {
	if sts == nil || sts.CreationTimestamp.IsZero() || len(sts.Spec.Template.Spec.Containers) == 0 {
		return ""
	}
	return sts.Spec.Template.Spec.Containers[0].Image
}

// imageToRun decides which image the Pod template carries. A new cluster gets
// the image its spec, or the operator's default, names. On an existing
// cluster a different image is taken over only when the user accepted exactly
// that image with the accept-image annotation; until then the template keeps
// the image it has, so a Pod that is recreated does not start one nobody
// accepted (ADR-0009). The operator cannot tell which engine a new image
// carries without running it, so the decision is the user's.
func (r *CubridClusterReconciler) imageToRun(cluster *databasev1alpha1.CubridCluster, sts *appsv1.StatefulSet) string {
	desired := r.instanceImage(cluster)
	running := runningImage(sts)
	if running == "" || running == desired || cluster.Annotations[acceptImageAnnotation] == desired {
		return desired
	}
	return running
}

func (r *CubridClusterReconciler) podSpec(cluster *databasev1alpha1.CubridCluster, image string) corev1.PodSpec {
	runAsNonRoot := true
	noPrivEscalation := false
	gracePeriod := int64(120)
	uid := cubridUID
	fsGroupPolicy := corev1.FSGroupChangeOnRootMismatch
	mounts := []corev1.VolumeMount{{Name: "data", MountPath: dataMountPath}}
	var volumes []corev1.Volume
	if cluster.Spec.HighAvailability.Enabled {
		// The same cubrid_ha.conf for every member (ADR-0004).
		volumes = append(volumes, haConfVolume(cluster))
		mounts = append(mounts, haConfMount())
	}
	// Pod Security Standards "restricted" (#18).
	security := &corev1.SecurityContext{
		RunAsNonRoot:             &runAsNonRoot,
		AllowPrivilegeEscalation: &noPrivEscalation,
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
	var initContainers []corev1.Container
	if cluster.Spec.HighAvailability.Enabled && networkPolicyEnabled(cluster) {
		initContainers = append(initContainers, peerWaitContainer(cluster, image, security))
	}
	return corev1.PodSpec{
		Volumes:        volumes,
		InitContainers: initContainers,
		// terminationGracePeriodSeconds >= 120s for ordered HA shutdown (ADR-0003).
		TerminationGracePeriodSeconds: &gracePeriod,
		SecurityContext: &corev1.PodSecurityContext{
			RunAsNonRoot: &runAsNonRoot,
			// The official image's user is root; run as its cubrid user instead,
			// and let fsGroup make the data PVC writable by that user.
			RunAsUser:           &uid,
			RunAsGroup:          &uid,
			FSGroup:             &uid,
			FSGroupChangePolicy: &fsGroupPolicy,
			SeccompProfile:      &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		Containers: []corev1.Container{{
			Name:      appName,
			Image:     image,
			Env:       instanceManagerEnv(cluster),
			Resources: cluster.Spec.Resources,
			// preStop triggers the ADR-0003 ordered graceful shutdown via the
			// local Instance Manager (loopback is token-exempt).
			Lifecycle: preStopShutdown(cluster),
			Ports: []corev1.ContainerPort{
				{Name: "cubrid", ContainerPort: cubridServerPort},
				{Name: "broker", ContainerPort: cubridBrokerPort},
				{Name: "manager", ContainerPort: instanceManagerPort},
			},
			// Readiness reflects DB-instance readiness only (Instance Manager
			// /readyz). Cluster HAReady is a SEPARATE Condition and must never
			// gate Pod readiness, else failover instability evicts every pod
			// from Services (ADR-0003/0005, #14).
			// The entrypoint creates and starts the database before the manager
			// listens, so nothing answers /livez during the first start. The
			// startup probe gives it ten minutes; liveness applies only after.
			StartupProbe: &corev1.Probe{
				ProbeHandler:     httpGet("/livez"),
				PeriodSeconds:    10,
				FailureThreshold: 60,
			},
			LivenessProbe: &corev1.Probe{
				ProbeHandler:     httpGet("/livez"),
				PeriodSeconds:    10,
				FailureThreshold: 6,
			},
			ReadinessProbe: &corev1.Probe{
				ProbeHandler:        httpGet("/readyz"),
				InitialDelaySeconds: 10,
				PeriodSeconds:       5,
				// /readyz runs a CUBRID command; the default of one second
				// would cancel it under load.
				TimeoutSeconds:   5,
				FailureThreshold: 3,
			},
			VolumeMounts:    mounts,
			SecurityContext: security,
		}},
	}
}

// instanceManagerEnv is the environment the Instance Manager image's
// entrypoint reads (build/instance-manager/entrypoint.sh, ADR-0003).
func instanceManagerEnv(cluster *databasev1alpha1.CubridCluster) []corev1.EnvVar {
	db := ""
	if len(cluster.Spec.Databases) > 0 {
		db = cluster.Spec.Databases[0].Name
	}
	// A single member runs a plain server. HA members are started by the HA
	// bootstrap (#106), which sets their role.
	components := "SERVER"
	if cluster.Spec.Topology.PromotableMembers > 1 {
		components = "HA"
	}
	// In recovery the database comes from a backup (ADR-0008): the entrypoint
	// must not create an empty one first.
	bootstrap := "new"
	if cluster.Spec.Bootstrap != nil && cluster.Spec.Bootstrap.Recovery != nil {
		bootstrap = "recovery"
	}
	return append([]corev1.EnvVar{
		{Name: "CUBRID_DB", Value: db},
		{Name: "CUBRID_DATABASES", Value: restoreTargetRoot},
		{Name: "CUBRID_COMPONENTS", Value: components},
		{Name: "CUBRID_BOOTSTRAP", Value: bootstrap},
		{Name: "IM_TOKEN", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: imTokenSecretName(cluster.Name)},
			Key:                  imTokenKey,
		}}},
		// Operation records live on the data volume so they survive a restart;
		// without this the manager serves no asynchronous backup or restore.
		{Name: "IM_OPERATIONS_DIR", Value: operationsDir},
		// The manager confines every staging path to these roots; they are the
		// ones the operator builds its requests with.
		{Name: "IM_BACKUP_STAGING_ROOT", Value: backupStagingRoot},
		{Name: "IM_RESTORE_STAGING_ROOT", Value: restoreStagingRoot},
		// Where the entrypoint and the manager find cubrid_ha.conf. The file is
		// there only for an HA cluster.
		{Name: "CUBRID_HA_CONF", Value: haConfMountPath + "/" + haConfFileName},
	}, objectStorageEnv(cluster.Spec.ObjectStorage)...)
}

// Keys of the Secret that spec.objectStorage.credentialsSecretRef names.
const (
	objectStorageAccessKey = "accessKey"
	objectStorageSecretKey = "secretKey"
)

// objectStorageEnv is the object-storage part of the manager's environment
// (ADR-0007: credentials come only from the manager's environment). The
// credentials are passed as references to the user's Secret; the operator
// never reads them.
func objectStorageEnv(storage *databasev1alpha1.CubridObjectStorage) []corev1.EnvVar {
	if storage == nil {
		return nil
	}
	fromSecret := func(key string) *corev1.EnvVarSource {
		return &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: storage.CredentialsSecretRef,
			Key:                  key,
		}}
	}
	env := make([]corev1.EnvVar, 0, 5)
	env = append(env,
		corev1.EnvVar{Name: "IM_S3_ENDPOINT", Value: storage.Endpoint},
		corev1.EnvVar{Name: "IM_S3_ACCESS_KEY", ValueFrom: fromSecret(objectStorageAccessKey)},
		corev1.EnvVar{Name: "IM_S3_SECRET_KEY", ValueFrom: fromSecret(objectStorageSecretKey)},
	)
	if storage.Region != "" {
		env = append(env, corev1.EnvVar{Name: "IM_S3_REGION", Value: storage.Region})
	}
	if storage.Insecure {
		env = append(env, corev1.EnvVar{Name: "IM_S3_INSECURE", Value: "true"})
	}
	return env
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func (r *CubridClusterReconciler) event(cluster *databasev1alpha1.CubridCluster, eventType, reason, msg string) {
	if r.Recorder != nil {
		r.Recorder.Event(cluster, eventType, reason, msg)
	}
}

func httpGet(path string) corev1.ProbeHandler {
	return corev1.ProbeHandler{
		HTTPGet: &corev1.HTTPGetAction{Path: path, Port: intOrString(instanceManagerPort)},
	}
}

// preStopShutdown runs the one command that stops a member, the same the
// entrypoint runs on a termination signal (ADR-0003). The command asks the
// local Instance Manager, which stops CUBRID once however often it is asked.
func preStopShutdown(_ *databasev1alpha1.CubridCluster) *corev1.Lifecycle {
	return &corev1.Lifecycle{
		PreStop: &corev1.LifecycleHandler{
			Exec: &corev1.ExecAction{
				Command: []string{"/bin/sh", "-c", instanceManagerBinary + " shutdown || true"},
			},
		},
	}
}

// updateStatus computes Conditions from the StatefulSet's observed state.
func (r *CubridClusterReconciler) updateStatus(ctx context.Context, cluster *databasev1alpha1.CubridCluster, sts *appsv1.StatefulSet) (ctrl.Result, error) {
	desired := cluster.Spec.Topology.PromotableMembers
	ready := sts.Status.ReadyReplicas

	cluster.Status.ObservedGeneration = cluster.Generation
	// Several steps below set Updating; only the value it ends with is compared
	// with the one it had, so a value held in between is no transition.
	updatingBefore := meta.FindStatusCondition(cluster.Status.Conditions, conditionUpdating).DeepCopy()

	// Recovery bootstrap gates Ready: while restoring, the operator must never
	// advertise a half-restored DB as healthy (ADR-0008).
	if recoveryActive(cluster) {
		res, active := r.reconcileRecovery(ctx, cluster)
		if active {
			reason, msg := "BootstrapRecoveryInProgress", "restoring from backup"
			if cluster.Status.Bootstrap.Phase == databasev1alpha1.BootstrapSeedingReplicas {
				reason, msg = "RecoverySeedingReplicas", "copying the restored database to the other members"
			}
			setCondition(cluster, conditionReady, metav1.ConditionFalse, reason, msg+"; cluster not ready")
			setCondition(cluster, conditionProgressing, metav1.ConditionTrue, reason, msg)
			markRolesNotObserved(cluster, reason)
			if err := r.Status().Update(ctx, cluster); err != nil {
				if apierrors.IsConflict(err) {
					return ctrl.Result{RequeueAfter: time.Second}, nil
				}
				return ctrl.Result{}, err
			}
			return res, nil
		}
	}

	// Read before the condition is set below, so the event marks the transition.
	wasReady := meta.IsStatusConditionTrue(cluster.Status.Conditions, conditionReady)
	if ready >= desired && desired > 0 {
		setCondition(cluster, conditionReady, metav1.ConditionTrue, "ClusterReady",
			fmt.Sprintf("%d/%d instances ready", ready, desired))
		setCondition(cluster, conditionProgressing, metav1.ConditionFalse, "Reconciled", "cluster reconciled")
		if !wasReady {
			r.event(cluster, corev1.EventTypeNormal, "ClusterReady",
				fmt.Sprintf("all %d instances ready", desired))
		}
	} else {
		setCondition(cluster, conditionReady, metav1.ConditionFalse, "InstancesNotReady",
			fmt.Sprintf("%d/%d instances ready", ready, desired))
		setCondition(cluster, conditionProgressing, metav1.ConditionTrue, "InstancesStarting",
			fmt.Sprintf("waiting for %d/%d instances", ready, desired))
	}

	labels := prometheus.Labels{"namespace": cluster.Namespace, "cluster": cluster.Name}
	metrics.ClusterInstances.With(labels).Set(float64(desired))
	metrics.InstanceReady.With(labels).Set(float64(ready))
	metrics.ClusterReady.With(labels).Set(boolToFloat(ready >= desired && desired > 0))

	// HAReady is separate from Ready and from Pod readiness (#14). Role
	// discovery polls each member's Instance Manager /v1/role (ADR-0003/0005).
	if !cluster.Spec.HighAvailability.Enabled {
		setCondition(cluster, conditionHAReady, metav1.ConditionFalse, "HADisabled",
			"highAvailability.enabled is false")
	} else if r.Prober == nil {
		setCondition(cluster, conditionHAReady, metav1.ConditionUnknown, "RoleDiscoveryDisabled",
			"no role prober configured")
	} else {
		// The first database is created once, on one member (ADR-0010).
		r.reconcileHABootstrap(ctx, cluster)
		res := r.reconcileHAStatus(ctx, cluster, desired)
		// Reconcile the broker tier and set routing conditions from the same
		// safety-first primary resolution (ADR-0002/0005).
		// A conflicting write leaves the broker conditions as they are; the
		// next reconcile writes again.
		if err := r.reconcileBrokerTier(ctx, cluster, res, r.imageToRun(cluster, sts)); apierrors.IsConflict(err) {
			logf.FromContext(ctx).V(1).Info("Retrying broker tier after a conflicting write", "error", err.Error())
		} else if err != nil {
			logf.FromContext(ctx).Error(err, "Failed to reconcile broker tier")
			setCondition(cluster, conditionBrokerReady, metav1.ConditionFalse, "BrokerReconcileFailed", err.Error())
		}
		// Rolling update (ADR-0009): only when the engine-version guard permits
		// (no blocked upgrade / unverifiable version), roll outdated slaves one at
		// a time, gated on the same primary resolution. Never during recovery.
		if r.reconcileUpdateGuard(cluster) && !recoveryActive(cluster) {
			r.reconcileRollingUpdate(ctx, cluster, sts, res)
		}
	}

	// An image the spec asks for but nobody accepted is reported last, so that
	// it is what the Updating condition says.
	if running, desired := runningImage(sts), r.instanceImage(cluster); running != "" && running != desired {
		setCondition(cluster, conditionUpdating, metav1.ConditionFalse, "ImageChangeNotAccepted",
			"the spec asks for image "+desired+"; the Pods keep "+running+" until the new image is accepted with the annotation "+
				acceptImageAnnotation+": "+desired)
	}
	if updating := meta.FindStatusCondition(cluster.Status.Conditions, conditionUpdating); updating != nil &&
		updatingBefore != nil && updating.Status == updatingBefore.Status {
		updating.LastTransitionTime = updatingBefore.LastTransitionTime
	}

	if err := r.Status().Update(ctx, cluster); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		return ctrl.Result{}, err
	}
	// A CUBRID failover changes no Kubernetes object, and /readyz answers 200
	// for a master and a slave alike, so no event would bring the new roles in.
	if cluster.Spec.HighAvailability.Enabled && r.Prober != nil {
		return ctrl.Result{RequeueAfter: haResyncInterval}, nil
	}
	return ctrl.Result{}, nil
}

// failed records a failure condition and returns the error for requeue. A
// conflict means the object was read before someone else changed it, which
// says nothing about the cluster: it is retried with the conditions and
// Events left as they are.
func (r *CubridClusterReconciler) failed(ctx context.Context, cluster *databasev1alpha1.CubridCluster, reason string, cause error) (ctrl.Result, error) {
	if apierrors.IsConflict(cause) {
		logf.FromContext(ctx).V(1).Info("Retrying after a conflicting write", "reason", reason, "error", cause.Error())
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	setCondition(cluster, conditionReady, metav1.ConditionFalse, reason, cause.Error())
	setCondition(cluster, conditionProgressing, metav1.ConditionTrue, reason, cause.Error())
	markRolesNotObserved(cluster, reason)
	r.event(cluster, corev1.EventTypeWarning, reason, cause.Error())
	// Best-effort status update; return the original cause for requeue.
	_ = r.Status().Update(ctx, cluster)
	return ctrl.Result{}, cause
}

func setCondition(cluster *databasev1alpha1.CubridCluster, condType string, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: cluster.Generation,
	})
}

func intOrString(port int32) intstr.IntOrString { return intstr.FromInt32(port) }

// clusterEvents passes the changes of a CubridCluster that a reconcile has to
// act on: its creation and deletion, a spec change (a new generation), an
// annotation change such as the image acceptance, which leaves the generation
// as it is, and the start of its deletion. A change of the status alone is
// not passed: the reconcile writes the status itself, and the counters it
// records differ on every write, so each write would start the next pass at
// once. An HA cluster is observed again every haResyncInterval instead, and
// the owned objects' own events still start a reconcile.
var clusterEvents = predicate.Or[client.Object](
	predicate.GenerationChangedPredicate{},
	predicate.AnnotationChangedPredicate{},
	predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
		return (e.ObjectOld.GetDeletionTimestamp() == nil) != (e.ObjectNew.GetDeletionTimestamp() == nil)
	}},
)

// SetupWithManager sets up the controller with the Manager.
func (r *CubridClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&databasev1alpha1.CubridCluster{}, builder.WithPredicates(clusterEvents)).
		Owns(&appsv1.StatefulSet{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Secret{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Named("cubridcluster").
		Complete(r)
}
