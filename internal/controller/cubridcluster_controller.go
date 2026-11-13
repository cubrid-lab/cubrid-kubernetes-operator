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
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

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
	// imTokenRotationDeferredAnnotation on the Secret is the requested
	// rotation that was last reported as waiting, so that it is reported once.
	imTokenRotationDeferredAnnotation = "database.cubrid.io/im-token-rotation-deferred"

	// Condition types (ADR-0005/0006).
	conditionReady       = "Ready"
	conditionProgressing = "Progressing"
	conditionHAReady     = "HAReady"

	appName = "cubrid"

	// ManagedByLabel and ManagedByOperator mark every object the operator
	// writes and every Pod of its workloads. The manager caches no object of a
	// kind it writes without them.
	ManagedByLabel    = "app.kubernetes.io/managed-by"
	ManagedByOperator = "cubrid-kubernetes-operator"
)

// CubridClusterReconciler reconciles a CubridCluster object.
type CubridClusterReconciler struct {
	client.Client
	// APIReader reads from the API server, past the manager's cache, which
	// holds only labelled Secrets. Nil reads through Client.
	APIReader client.Reader
	Scheme    *runtime.Scheme
	Recorder  record.EventRecorder
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
	// BrokerMaxApplServers is every Broker's MAX_NUM_APPL_SERVER; zero means
	// DefaultBrokerMaxApplServers. Changing it replaces the Brokers, as any
	// change of their configuration does.
	BrokerMaxApplServers int
	// Clock judges observation freshness; nil means time.Now.
	Clock func() time.Time
	// BackupTimeout and RestoreTimeout are how long the Instance Manager lets
	// a backup (backupdb and the upload) and a restore (the download and
	// restoredb) run before it ends them. Zero leaves the manager's own
	// default, two hours each.
	BackupTimeout, RestoreTimeout time.Duration
	// OperatorNS is the namespace of the operator's Pods, the only Pods the
	// NetworkPolicy of the DB Pods admits to the Instance Manager port. Empty
	// admits none.
	OperatorNS string
	// Positions keeps the last reading of each slave's applier for the stall
	// rule (nextReplication), shared with the CubridBackupReconciler. Nil
	// keeps the readings in a store of this reconciler's own.
	Positions    *ApplierPositions
	ownPositions ApplierPositions
	// DBAPasswordRecheck is how long a cluster whose DBA password Secret does
	// not resolve waits before it is checked again; zero is
	// defaultDBAPasswordRecheck. The operator does not watch the user's
	// Secrets, so this check is what lets the cluster go on once the Secret
	// is created or corrected.
	DBAPasswordRecheck time.Duration
	// DBAPassword asks the member of a cluster with a DBA password Secret
	// whether the Secret's value is still the database's DBA password. Nil
	// asks nothing, and a changed value is not reported.
	DBAPassword DBAPasswordChecker
	// DBAPasswordPoll is how often that is asked while nothing else brings
	// the cluster back; zero is defaultDBAPasswordPoll.
	DBAPasswordPoll time.Duration

	// reports holds, per cluster, the report of the reconcile that is running
	// for it (reconcileReport).
	reports sync.Map
	// conflicts holds, per cluster and group of writes, since when its writes
	// have been rejected as a conflict without one that succeeded.
	conflicts sync.Map
	// placementReported holds, per cluster, the last MemberPlacement reason
	// that was True or False when its status was written (placementEntry),
	// so that an Unknown in between records no Event again.
	placementReported sync.Map
	// unresolvedSince holds, per cluster, since when PrimaryResolved has not
	// been True (unresolvedStart).
	unresolvedSince sync.Map
	// dbaPasswordCheckedAt holds, per cluster UID, when its member was last
	// asked whether the DBA password Secret's value is the database's
	// password (reconcileDBAPasswordRotation), so that a burst of reconciles
	// does not log in to the database once each.
	dbaPasswordCheckedAt sync.Map
}

// conflictKey names one group of a cluster's writes in conflicts.
type conflictKey struct {
	cluster types.NamespacedName
	writes  string
}

const (
	childWrites  = "children"
	brokerWrites = "brokers"
)

// lastingConflict tells what to do with the error of a write. A conflict is
// retried quietly, and the first return value is true, until writes of this
// group have been rejected for longer than conflictPatience; from then on the
// second return value is the error to report. Any other error ends the run of
// conflicts and is returned as it is.
func (r *CubridClusterReconciler) lastingConflict(cluster *databasev1alpha1.CubridCluster, writes string, err error) (bool, error) {
	key := conflictKey{client.ObjectKeyFromObject(cluster), writes}
	if !apierrors.IsConflict(err) {
		r.conflicts.Delete(key)
		return false, err
	}
	since, _ := r.conflicts.LoadOrStore(key, r.now())
	if r.now().Sub(since.(time.Time)) <= conflictPatience {
		return true, nil
	}
	return false, fmt.Errorf("the write has been rejected as a conflict for more than %s: %w", conflictPatience, err)
}

// conflictPatience is how long a write of one of the cluster's objects may
// keep being rejected as a conflict before it is reported like any other
// failed write. A conflict that another writer keeps causing would otherwise
// be retried without end and without a trace in the status (#361).
const conflictPatience = 30 * time.Second

// reconcileReport is what one reconcile of one cluster has to tell about a
// change of the cluster's state. A change of state is a change of the status,
// so its Events are kept here and recorded only when that status was written:
// an Event for a status that was not written would be recorded again by the
// reconcile that writes it (docs/observability.md).
type reconcileReport struct {
	// before is the status the reconcile started from, as it is stored.
	// writeStatus compares with it to hold observation times back, so a
	// write retried after a conflict (RetryOnConflict) must read the cluster
	// again and replace it, or the comparison is with an outdated status.
	before *databasev1alpha1.CubridClusterStatus
	// statusWritten says that the last write of the status succeeded.
	statusWritten bool
	events        []pendingEvent
	// readyReplicas is the number of ready Pods of the StatefulSet this
	// reconcile read, nil when it read none.
	readyReplicas *int32
	// positions are the applier readings of this reconcile's observation,
	// kept in ApplierPositions once a write of the status they fed succeeds;
	// nil when the roles were not observed.
	positions map[string]applierReading
	// placement is the MemberPlacement reason this reconcile reported, kept
	// in placementReported once the status that carries it is written.
	placement *placementEntry
	// brokerRefused says that the Broker tier met an object the cluster does
	// not control.
	brokerRefused bool
}

type pendingEvent struct{ eventType, reason, message string }

// beginReport starts the report of a reconcile of cluster. One reconcile runs
// per cluster at a time.
func (r *CubridClusterReconciler) beginReport(cluster *databasev1alpha1.CubridCluster) *reconcileReport {
	report := &reconcileReport{before: cluster.Status.DeepCopy()}
	r.reports.Store(client.ObjectKeyFromObject(cluster), report)
	return report
}

// endReport records what the reconcile found, when the status that carries
// it was written: the Events that were kept, then the transitions between the
// status before and now. Otherwise nothing is recorded or logged as a
// transition; the next reconcile starts from the status that is stored and
// finds the same change.
func (r *CubridClusterReconciler) endReport(ctx context.Context, cluster *databasev1alpha1.CubridCluster,
	before *databasev1alpha1.CubridClusterStatus, report *reconcileReport) {
	r.reports.Delete(client.ObjectKeyFromObject(cluster))
	if !report.statusWritten {
		if pending := len(report.events) + len(transitions(before, &cluster.Status)); pending > 0 {
			logf.FromContext(ctx).V(1).Info("Left changes of state unreported because the status was not written",
				"namespace", cluster.Namespace, "cluster", cluster.Name, "changes", pending)
		}
		return
	}
	for _, e := range report.events {
		r.event(cluster, e.eventType, e.reason, e.message)
	}
	if report.placement != nil {
		r.placementReported.Store(client.ObjectKeyFromObject(cluster), *report.placement)
	}
	r.reportTransitions(ctx, cluster, before)
}

// statusEvent is event for a change that the status carries: it is recorded
// when the reconcile ends, if the status was written. Outside a reconcile it
// is recorded at once.
func (r *CubridClusterReconciler) statusEvent(cluster *databasev1alpha1.CubridCluster, eventType, reason, msg string) {
	if report, ok := r.reports.Load(client.ObjectKeyFromObject(cluster)); ok {
		report := report.(*reconcileReport)
		report.events = append(report.events, pendingEvent{eventType, reason, msg})
		return
	}
	r.event(cluster, eventType, reason, msg)
}

// writeStatus writes the cluster's status and notes for the reconcile's
// report whether it was written. A status whose only change from the stored
// one is a newer observation time keeps the stored times while they are
// younger than observationWriteInterval (holdObservationTimes), so that the
// API server finds nothing to write.
func (r *CubridClusterReconciler) writeStatus(ctx context.Context, cluster *databasev1alpha1.CubridCluster) error {
	report, ok := r.reports.Load(client.ObjectKeyFromObject(cluster))
	if ok {
		holdObservationTimes(report.(*reconcileReport).before, &cluster.Status, r.now())
	}
	err := r.Status().Update(ctx, cluster)
	var readyReplicas *int32
	if ok {
		report.(*reconcileReport).statusWritten = err == nil
		readyReplicas = report.(*reconcileReport).readyReplicas
	}
	if err == nil {
		// The metrics say what the stored status says.
		publishClusterMetrics(cluster, readyReplicas)
		// So do the applier readings the next one is compared with.
		if ok && report.(*reconcileReport).positions != nil {
			r.applierPositions().commit(cluster.UID, client.ObjectKeyFromObject(cluster), report.(*reconcileReport).positions)
		}
	}
	if apierrors.IsNotFound(err) {
		// The cluster was deleted during the reconcile: there is nothing to
		// write the status to, and nothing to retry. Any other NotFound, such
		// as a CRD without the status subresource, stays an error.
		gone := r.Get(ctx, client.ObjectKeyFromObject(cluster), &databasev1alpha1.CubridCluster{})
		if apierrors.IsNotFound(gone) {
			r.applierPositions().forget(client.ObjectKeyFromObject(cluster))
			logf.FromContext(ctx).V(1).Info("Skipped status write of a deleted CubridCluster")
			return nil
		}
	}
	return err
}

// namespaceTerminating reports whether the API server refused a write because
// the namespace is being deleted. The namespace's objects go with it, so the
// refusal is no failure of the cluster.
func namespaceTerminating(err error) bool {
	return apierrors.HasStatusCause(err, corev1.NamespaceTerminatingCause)
}

func (r *CubridClusterReconciler) now() time.Time {
	if r.Clock != nil {
		return r.Clock()
	}
	return time.Now()
}

// +kubebuilder:rbac:groups=database.cubrid.io,resources=cubridclusters,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=database.cubrid.io,resources=cubridclusters/status,verbs=update
// +kubebuilder:rbac:groups=database.cubrid.io,resources=cubridclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update

// Reconcile drives the CubridCluster toward its desired state: a headless
// governing Service and a StatefulSet with a data PVC template; HA role
// discovery + safety-first status (ADR-0005); the broker tier (ADR-0002);
// recovery-bootstrap orchestration (ADR-0008); and the engine-version guard +
// slaves-first rolling update (ADR-0009).
func (r *CubridClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var cluster databasev1alpha1.CubridCluster
	if err := r.Get(ctx, req.NamespacedName, &cluster); err != nil {
		// Ignore not-found: the object was deleted; owned resources are GC'd
		// via owner references.
		r.conflicts.Delete(conflictKey{req.NamespacedName, childWrites})
		r.conflicts.Delete(conflictKey{req.NamespacedName, brokerWrites})
		r.placementReported.Delete(req.NamespacedName)
		r.unresolvedSince.Delete(req.NamespacedName)
		if apierrors.IsNotFound(err) {
			metrics.DeleteCluster(req.Namespace, req.Name)
			r.applierPositions().forget(req.NamespacedName)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// What changed in the status during this reconcile is reported once, when
	// it ends, if the status was written (docs/observability.md).
	report := r.beginReport(&cluster)
	defer func() { r.endReport(ctx, &cluster, report.before, report) }()

	// A name no child object can be built from is refused before any is
	// written; it cannot change, so it is not retried.
	if err := validateManagedClusterName(cluster.Name); err != nil {
		return r.refuseClusterName(ctx, &cluster, err)
	}

	// A write quarantine in the status is enforced before anything else, from
	// the status alone, so that no later step or failure of this pass, and no
	// restart of the Operator, lets the read-write Brokers run (ADR-0005,
	// section 8).
	if err := r.enforceWriteQuarantine(ctx, &cluster); err != nil {
		return r.failed(ctx, &cluster, reasonWriteQuarantineEnforcementFailed, err)
	}

	// Reconcile the governing headless Service (ADR-0004: stable per-pod DNS).
	if err := r.reconcileHeadlessService(ctx, &cluster); err != nil {
		return r.failed(ctx, &cluster, "ServiceReconcileFailed", err)
	}

	// One alias Service per member, so its short HA host name resolves.
	if err := r.reconcileMemberServices(ctx, &cluster); err != nil {
		return r.failed(ctx, &cluster, "MemberServiceReconcileFailed", err)
	}

	// The DB Pods authenticate the operator with the cluster's own token
	// (ADR-0003), which exists before the first Pod does.
	if err := r.reconcileIMTokenSecret(ctx, &cluster); errors.Is(err, errIMTokenSecretNotCached) {
		logf.FromContext(ctx).V(1).Info("Retrying until the cache holds the token Secret")
		return ctrl.Result{RequeueAfter: time.Second}, nil
	} else if err != nil {
		return r.failed(ctx, &cluster, "SecretReconcileFailed", err)
	}

	// The HA member list every member reads (ADR-0004); none when standalone.
	if err := r.reconcileHAConfig(ctx, &cluster); err != nil {
		return r.failed(ctx, &cluster, "HAConfigReconcileFailed", err)
	}

	// The ingress NetworkPolicies exist before the first DB Pod does.
	if err := r.reconcileNetworkPolicies(ctx, &cluster); err != nil {
		return r.failed(ctx, &cluster, "NetworkPolicyReconcileFailed", err)
	}

	// The member spread an unset spec falls back to is recorded before the
	// StatefulSet exists, so that it tells a new cluster from an old one.
	if err := r.recordMemberSpreadDefault(ctx, &cluster); err != nil {
		return r.failed(ctx, &cluster, "PlacementReconcileFailed", err)
	}

	// A database is never created without the DBA password its cluster
	// names: the StatefulSet waits for the Secret.
	if stop, recheck, err := r.reconcileDBAPasswordSecret(ctx, &cluster); stop {
		return ctrl.Result{RequeueAfter: recheck}, err
	}

	// Reconcile the StatefulSet (DB instances + per-ordinal data PVC).
	sts, err := r.reconcileStatefulSet(ctx, &cluster)
	if err != nil {
		return r.failed(ctx, &cluster, "StatefulSetReconcileFailed", err)
	}

	// The budgets that limit a node drain (docs/architecture/voluntary-disruption.md).
	if err := r.reconcileDisruptionBudgets(ctx, &cluster); err != nil {
		return r.failed(ctx, &cluster, "DisruptionBudgetReconcileFailed", err)
	}

	r.conflicts.Delete(conflictKey{req.NamespacedName, childWrites})

	return r.reconcileMembers(ctx, &cluster, sts)
}

// reportDeferredRotation records that a requested token rotation waits for
// the overlap to end. The request stays deferred over many reconciles, so it
// is noted on the Secret first and reported once.
func (r *CubridClusterReconciler) reportDeferredRotation(ctx context.Context, cluster *databasev1alpha1.CubridCluster,
	secret *corev1.Secret, request string) error {
	if request == secret.Annotations[imTokenRotationDeferredAnnotation] {
		return nil
	}
	if secret.Annotations == nil {
		secret.Annotations = map[string]string{}
	}
	secret.Annotations[imTokenRotationDeferredAnnotation] = request
	if err := r.Update(ctx, secret); err != nil {
		return err
	}
	r.event(cluster, corev1.EventTypeNormal, "InstanceManagerTokenRotationDeferred",
		fmt.Sprintf("the requested Instance Manager token rotation waits until every member holds the current token of Secret %s",
			secret.Name))
	return nil
}

// maxManagedClusterNameLength is the longest CubridCluster name the operator
// manages. It is one more than the admission limit
// databasev1alpha1.MaxClusterNameLength (52): the API accepted 53-character
// names before that limit existed, and every name and label value the
// operator writes for them still fits in 63 characters (the longest are
// <cluster>-instances and <cluster>-broker-rw/-ro). Only the StatefulSet
// controller's controller-revision-hash Pod label can exceed 63 characters
// for such a name, which is why new names are limited to 52 (ADR-0004).
const maxManagedClusterNameLength = 53

// reasonInvalidClusterName is the Ready reason of a CubridCluster whose name
// the operator refuses to manage.
const reasonInvalidClusterName = "InvalidClusterName"

// validateManagedClusterName checks that a cluster name is a DNS-1035 label
// of at most maxManagedClusterNameLength characters.
func validateManagedClusterName(name string) error {
	if len(name) > maxManagedClusterNameLength {
		return fmt.Errorf("cluster name %q has %d characters, more than the %d the operator can derive child object names from",
			name, len(name), maxManagedClusterNameLength)
	}
	if errs := validation.IsDNS1035Label(name); len(errs) > 0 {
		return fmt.Errorf("cluster name %q is not a DNS-1035 label: %s", name, strings.Join(errs, "; "))
	}
	return nil
}

// refuseClusterName reports a cluster name the operator cannot manage: Ready
// is False with reason InvalidClusterName, one Warning Event is recorded, and
// the returned terminal error is not retried. The name is immutable, so a
// retry cannot succeed.
func (r *CubridClusterReconciler) refuseClusterName(ctx context.Context, cluster *databasev1alpha1.CubridCluster,
	cause error) (ctrl.Result, error) {
	logf.FromContext(ctx).Error(cause, "Refused to manage a CubridCluster with an invalid name")
	msg := cause.Error() + "; recreate the cluster with a name of at most " +
		strconv.Itoa(databasev1alpha1.MaxClusterNameLength) + " characters"
	was := meta.FindStatusCondition(cluster.Status.Conditions, conditionReady)
	same := was != nil && was.Status == metav1.ConditionFalse && was.Reason == reasonInvalidClusterName && was.Message == msg
	setCondition(cluster, conditionReady, metav1.ConditionFalse, reasonInvalidClusterName, msg)
	setCondition(cluster, conditionProgressing, metav1.ConditionFalse, reasonInvalidClusterName, msg)
	r.markRolesNotObserved(cluster, reasonInvalidClusterName)
	if !same {
		r.statusEvent(cluster, corev1.EventTypeWarning, reasonInvalidClusterName, msg)
	}
	// A status that could not be written is retried: the refusal itself is
	// terminal, but its report would otherwise wait for the next change.
	if err := r.writeStatus(ctx, cluster); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, reconcile.TerminalError(cause)
}

// instancesServiceName is the governing headless Service (<cluster>-instances).
func instancesServiceName(name string) string { return name + "-instances" }

// labelsFor returns the standard selector labels for a cluster's DB instances.
func labelsFor(cluster *databasev1alpha1.CubridCluster) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":      appName,
		"app.kubernetes.io/instance":  cluster.Name,
		"app.kubernetes.io/component": "database",
		ManagedByLabel:                ManagedByOperator,
	}
}

func (r *CubridClusterReconciler) reconcileHeadlessService(ctx context.Context, cluster *databasev1alpha1.CubridCluster) error {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instancesServiceName(cluster.Name),
			Namespace: cluster.Namespace,
		},
	}
	return r.reconcileChild(ctx, cluster, svc, func() error {
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
		return nil
	})
}

// instanceManagerBinary is the Instance Manager in the image
// (build/instance-manager/Dockerfile).
const instanceManagerBinary = "/usr/local/bin/instance-manager"

func imTokenSecretName(cluster string) string { return cluster + "-im-token" }

// errIMTokenSecretNotCached is a create of the token Secret refused because
// the operator's own labelled Secret exists but is not in the cache yet.
var errIMTokenSecretNotCached = errors.New("the token Secret is not in the cache yet")

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
	err := r.reconcileChild(ctx, cluster, secret, func() error {
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
		// White space alone is no token: the manager refuses to start with it.
		case len(bytes.TrimSpace(secret.Data[imTokenKey])) == 0:
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
		return nil
	})
	if apierrors.IsAlreadyExists(err) {
		return r.imTokenSecretRefused(ctx, secret, err)
	}
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
			return r.reportDeferredRotation(ctx, cluster, secret, request)
		}
		return nil
	}
	// A new token under an existing StatefulSet: its running Pods still hold
	// the old one and refuse the operator until each of them is replaced.
	err = r.childClient().Get(ctx, client.ObjectKey{Name: cluster.Name, Namespace: cluster.Namespace}, &appsv1.StatefulSet{})
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

// imTokenSecretRefused explains a create of the token Secret refused as
// AlreadyExists. The manager caches only Secrets with the managed-by label, so
// a Secret of this name without it is not seen and the create is refused. So
// is the create of a labelled Secret the cache has not caught up with yet,
// which is no collision: deleting it would replace the token running Pods
// hold. An uncached read tells the two apart.
func (r *CubridClusterReconciler) imTokenSecretRefused(ctx context.Context, secret *corev1.Secret, err error) error {
	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}
	stored := &corev1.Secret{}
	readErr := reader.Get(ctx, client.ObjectKeyFromObject(secret), stored)
	switch {
	case readErr == nil && stored.Labels[ManagedByLabel] == ManagedByOperator:
		return errIMTokenSecretNotCached
	case readErr == nil:
		return fmt.Errorf("the Secret %s exists without the label %s=%s, so the operator neither reads nor adopts it; "+
			"delete or rename it: %w", secret.Name, ManagedByLabel, ManagedByOperator, err)
	default:
		return fmt.Errorf("the Secret %s exists but the operator cannot read it (%v); "+
			"if it lacks the label %s=%s, the operator neither reads nor adopts it: %w",
			secret.Name, readErr, ManagedByLabel, ManagedByOperator, err)
	}
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
	err := r.reconcileChild(ctx, cluster, sts, func() error {
		sts.Labels = labels
		// Immutable fields (selector, volumeClaimTemplates, serviceName) are only
		// set on creation; CreateOrUpdate skips them on update by leaving the
		// existing values in place for a freshly-fetched object.
		if sts.CreationTimestamp.IsZero() {
			sts.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
			sts.Spec.ServiceName = instancesServiceName(cluster.Name)
			// One claim per member; CUBRID's logs are on the data volume
			// (enginelogs.go, #856).
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
		spec := r.podSpec(cluster, r.imageToRun(cluster, sts))
		mountEngineLogs(sts, &spec)
		return setPodTemplate(sts, &sts.Spec.Template, corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: labels},
			Spec:       spec,
		})
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
		ObjectMeta: metav1.ObjectMeta{Name: dataVolume},
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
	mounts := []corev1.VolumeMount{{Name: dataVolume, MountPath: dataMountPath}}
	var volumes []corev1.Volume
	if cluster.Spec.HighAvailability.Enabled {
		// The same cubrid_ha.conf for every member (ADR-0004).
		volumes = append(volumes, haConfVolume(cluster))
		mounts = append(mounts, haConfMount())
	}
	env := append(instanceManagerEnv(cluster), r.timeLimitEnv()...)
	// Only the DB container mounts the DBA password, read-only.
	if ref := cluster.Spec.DBAPasswordSecretRef; ref != nil {
		volumes = append(volumes, dbaPasswordVolumeFor(ref))
		mounts = append(mounts, corev1.VolumeMount{Name: dbaPasswordVolume, MountPath: dbaPasswordMountPath, ReadOnly: true})
		env = append(env, dbaPasswordEnv())
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
		Affinity:       memberAffinity(cluster),
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
			Env:       env,
			Resources: cluster.Spec.Resources,
			// preStop triggers the ADR-0003 ordered graceful shutdown via the
			// local Instance Manager; the command sends the Pod's IM_TOKEN.
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
		// The size of each log file of a database the member creates; it is
		// fixed when the database is created (#666).
		{Name: "CUBRID_LOG_VOLUME_SIZE", Value: logVolumeSize(cluster.Spec.Storage.Data.Size)},
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

// timeLimitEnv tells the Instance Manager how long a backup and a restore may
// run, when the operator was given such limits. An unset one adds nothing, so
// the manager keeps its default and the Pod template of a cluster stays as it
// is. A limit is part of the Pod template: a running Pod keeps the one it
// started with until it is replaced.
func (r *CubridClusterReconciler) timeLimitEnv() []corev1.EnvVar {
	var env []corev1.EnvVar
	if r.BackupTimeout > 0 {
		env = append(env, corev1.EnvVar{Name: "IM_BACKUP_TIMEOUT", Value: r.BackupTimeout.String()})
	}
	if r.RestoreTimeout > 0 {
		env = append(env, corev1.EnvVar{Name: "IM_RESTORE_TIMEOUT", Value: r.RestoreTimeout.String()})
	}
	return env
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

// stepResult is what a step that writes child objects leaves for the end of
// the reconcile. The status is written first, so that the Conditions that
// explain a failed write reach the API server; err is then returned, and
// controller-runtime retries the reconcile with backoff. retry, when not zero,
// is the longest the reconcile waits before it runs again, as after a write
// rejected as a conflict that is still young (lastingConflict).
type stepResult struct {
	retry time.Duration
	err   error
}

// reconcileMembers runs the steps that act on the members and on the Broker
// tier, in this order, and then writes the status (updateStatus):
//
//   - a recovery bootstrap that has not completed (reconcileRecovery), which
//     ends the reconcile with its own status (ADR-0008);
//   - for a standalone cluster, the engine guard, the Broker tier and the
//     check of its DBA password Secret (reconcileStandaloneMembers);
//   - for an HA cluster with a role prober, the HA bootstrap, the role
//     observation, the Broker tier and the rolling update
//     (reconcileHAMembers).
//
// Each step sets the Conditions its outcome says, in the order the steps run.
func (r *CubridClusterReconciler) reconcileMembers(ctx context.Context, cluster *databasev1alpha1.CubridCluster,
	sts *appsv1.StatefulSet) (ctrl.Result, error) {
	ready := sts.Status.ReadyReplicas
	if report, ok := r.reports.Load(client.ObjectKeyFromObject(cluster)); ok {
		report.(*reconcileReport).readyReplicas = &ready
	}

	cluster.Status.ObservedGeneration = cluster.Generation
	// Where the members run is reported in every phase, recovery included;
	// it is a report only (#596).
	r.reportMemberPlacement(ctx, cluster)
	// Several steps below set Updating; only the value it ends with is compared
	// with the one it had, so a value held in between is no transition.
	updatingBefore := meta.FindStatusCondition(cluster.Status.Conditions, conditionUpdating).DeepCopy()

	// Recovery bootstrap gates Ready: while restoring, the operator must never
	// advertise a half-restored DB as healthy (ADR-0008).
	if recoveryActive(cluster) {
		if res, active := r.reconcileRecovery(ctx, cluster); active {
			return r.updateRecoveryStatus(ctx, cluster, res)
		}
	}

	// HAReady is separate from Ready and from Pod readiness (#14). Role
	// discovery polls each member's Instance Manager /v1/role (ADR-0003/0005).
	// Ready is decided after it, because two observed masters keep Ready from
	// True.
	multiplePrimaries := false
	var steps stepResult
	switch {
	case !cluster.Spec.HighAvailability.Enabled:
		steps = r.reconcileStandaloneMembers(ctx, cluster, sts)
	case r.Prober == nil:
		setCondition(cluster, conditionHAReady, metav1.ConditionUnknown, "RoleDiscoveryDisabled",
			"no role prober configured")
	default:
		multiplePrimaries, steps = r.reconcileHAMembers(ctx, cluster, sts)
	}
	return r.updateStatus(ctx, cluster, sts, updatingBefore, multiplePrimaries, steps)
}

// updateRecoveryStatus writes the status of a cluster whose recovery
// bootstrap has not completed: Ready is False and says how far the recovery
// is. res is what the recovery step asks the reconcile to return.
func (r *CubridClusterReconciler) updateRecoveryStatus(ctx context.Context, cluster *databasev1alpha1.CubridCluster,
	res ctrl.Result) (ctrl.Result, error) {
	reason, msg := "BootstrapRecoveryInProgress", "restoring from backup"
	progressing := metav1.ConditionTrue
	switch cluster.Status.Bootstrap.Phase {
	case databasev1alpha1.BootstrapSeedingReplicas:
		reason, msg = "RecoverySeedingReplicas", "copying the restored database to the other members"
	case databasev1alpha1.BootstrapFailed:
		// Terminal: nothing is in progress, and BootstrapReady says why.
		reason, msg, progressing = reasonBootstrapRecoveryFailed, "the recovery failed", metav1.ConditionFalse
		if c := meta.FindStatusCondition(cluster.Status.Conditions, conditionBootstrapReady); c != nil && c.Message != "" {
			msg += ": " + c.Message
		}
	}
	setCondition(cluster, conditionReady, metav1.ConditionFalse, reason, msg+"; cluster not ready")
	setCondition(cluster, conditionProgressing, progressing, reason, msg)
	r.markRolesNotObserved(cluster, reason)
	setRecoveryFailedHAReady(cluster, msg)
	if err := r.writeStatus(ctx, cluster); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		return ctrl.Result{}, err
	}
	return res, nil
}

// reconcileStandaloneMembers observes the member of a standalone cluster,
// applies the engine guard to it (ADR-0009), reconciles its Broker tier and
// asks whether its DBA password Secret still holds the database's password
// (reconcileDBAPasswordRotation).
func (r *CubridClusterReconciler) reconcileStandaloneMembers(ctx context.Context, cluster *databasev1alpha1.CubridCluster,
	sts *appsv1.StatefulSet) stepResult {
	desired := cluster.Spec.Topology.PromotableMembers
	setCondition(cluster, conditionHAReady, metav1.ConditionFalse, "HADisabled",
		"highAvailability.enabled is false")
	// The member has no HA role, but it reports its engine version, and
	// the same guard as for an HA cluster applies to it (ADR-0009).
	if r.Prober != nil {
		r.reconcileStandaloneUpdate(ctx, cluster, sts, desired)
	}
	// Its Brokers need no role prober: they connect to the one server,
	// whose readiness the Pod reports.
	available, ok, steps := r.brokerTier(ctx, cluster, sts, nil)
	if ok {
		setStandaloneBrokerConditions(cluster, sts.Status.ReadyReplicas >= desired && desired > 0, available)
	}
	// The sooner of the two retries is kept.
	if poll := r.reconcileDBAPasswordRotation(ctx, cluster, dbaPasswordStandaloneMember(cluster, sts)); poll > 0 &&
		(steps.retry == 0 || poll < steps.retry) {
		steps.retry = poll
	}
	return steps
}

// reconcileHAMembers runs the steps of an HA cluster with a role prober: the
// HA bootstrap (ADR-0010), the role observation, the Broker tier, the rolling
// update and the check of its DBA password Secret against its primary
// (reconcileDBAPasswordRotation). It reports whether more than one master was
// observed.
func (r *CubridClusterReconciler) reconcileHAMembers(ctx context.Context, cluster *databasev1alpha1.CubridCluster,
	sts *appsv1.StatefulSet) (bool, stepResult) {
	// The first database is created once, on one member (ADR-0010).
	r.reconcileHABootstrap(ctx, cluster)
	lastResolved := cluster.Status.LastResolvedPrimary
	res, obs, observedAt := r.observeHAStatus(ctx, cluster, cluster.Spec.Topology.PromotableMembers)
	// The write quarantine is decided from the same observation, before the
	// Broker tier, which keeps the read-write Brokers at zero while one is in
	// force (ADR-0005, section 8).
	quarantine := r.reconcileWriteQuarantine(ctx, cluster,
		memberNames(cluster, cluster.Spec.Topology.PromotableMembers), obs, observedAt, lastResolved)
	// Reconcile the broker tier and set routing conditions from the same
	// safety-first primary resolution (ADR-0002/0005).
	// A conflicting write leaves the broker conditions as they are; the
	// reconcile is retried soon and writes again.
	available, ok, steps := r.brokerTier(ctx, cluster, sts, &res)
	if ok {
		r.setBrokerConditions(cluster, res, available)
	}
	steps.err = errors.Join(quarantine.err, steps.err)
	if quarantine.retry > 0 && (steps.retry == 0 || quarantine.retry < steps.retry) {
		steps.retry = quarantine.retry
	}
	// Rolling update (ADR-0009): only when the engine-version guard permits
	// (no blocked upgrade / unverifiable version), roll outdated slaves one at
	// a time, gated on the same primary resolution. Never during recovery.
	if r.reconcileUpdateGuard(cluster) && !recoveryActive(cluster) {
		if err := r.reconcileRollingUpdate(ctx, cluster, sts, res); err != nil {
			steps.err = errors.Join(steps.err, err)
		}
	}
	// The sooner of the two retries is kept.
	if poll := r.reconcileDBAPasswordRotation(ctx, cluster, dbaPasswordHAMember(cluster, res)); poll > 0 &&
		(steps.retry == 0 || poll < steps.retry) {
		steps.retry = poll
	}
	return res.MultipleMasters, steps
}

// updateStatus sets Ready and the Updating condition of an image change from
// what the steps left, writes the status and decides what the reconcile
// returns. It writes no child object and calls no Instance Manager.
//
// A step's failed write is returned as the reconcile's error once the status
// that explains it is written, or together with the error of a status write
// that failed. Without one, the reconcile is retried after haResyncInterval
// for an HA cluster with a role prober, or else after idleRetry, unless a
// step asks to be retried sooner.
func (r *CubridClusterReconciler) updateStatus(ctx context.Context, cluster *databasev1alpha1.CubridCluster, sts *appsv1.StatefulSet,
	updatingBefore *metav1.Condition, multiplePrimaries bool, steps stepResult) (ctrl.Result, error) {
	r.setReady(cluster, sts.Status.ReadyReplicas, cluster.Spec.Topology.PromotableMembers, multiplePrimaries)

	// An image the spec asks for but nobody accepted is reported last, so that
	// it is what the Updating condition says.
	if running, desired := runningImage(sts), r.instanceImage(cluster); running != "" && running != desired {
		setCondition(cluster, conditionUpdating, metav1.ConditionFalse, reasonImageChangeNotAccepted,
			"the spec asks for image "+desired+"; the Pods keep "+running+" until the new image is accepted with the annotation "+
				acceptImageAnnotation+": "+desired)
	} else if updating := meta.FindStatusCondition(cluster.Status.Conditions, conditionUpdating); updating != nil &&
		updating.Reason == reasonImageChangeNotAccepted {
		// The Pod template carries the image the spec asks for, and no step
		// of this reconcile said anything else about the update: the held
		// image is no longer pending.
		meta.RemoveStatusCondition(&cluster.Status.Conditions, conditionUpdating)
	}
	if updating := meta.FindStatusCondition(cluster.Status.Conditions, conditionUpdating); updating != nil &&
		updatingBefore != nil && updating.Status == updatingBefore.Status {
		updating.LastTransitionTime = updatingBefore.LastTransitionTime
	}

	if err := r.writeStatus(ctx, cluster); err != nil {
		if steps.err != nil {
			return ctrl.Result{}, errors.Join(steps.err, err)
		}
		if apierrors.IsConflict(err) {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		return ctrl.Result{}, err
	}
	if steps.err != nil {
		return ctrl.Result{}, steps.err
	}
	result := ctrl.Result{RequeueAfter: r.idleRetry(cluster)}
	// A CUBRID failover changes no Kubernetes object, and /readyz answers 200
	// for a master and a slave alike, so no event would bring the new roles in.
	if cluster.Spec.HighAvailability.Enabled && r.Prober != nil {
		result.RequeueAfter = haResyncInterval
	}
	if steps.retry > 0 && (result.RequeueAfter == 0 || steps.retry < result.RequeueAfter) {
		result.RequeueAfter = steps.retry
	}
	return result, nil
}

// bootstrapProgress says where the HA bootstrap stands, in BootstrapReady's
// reason and message: it may copy a member, wait for its source or for a
// person, or be held by its configuration.
func bootstrapProgress(cluster *databasev1alpha1.CubridCluster) string {
	c := meta.FindStatusCondition(cluster.Status.Conditions, conditionBootstrapReady)
	if c == nil || c.Reason == "" {
		return "BootstrapReady is not reported yet"
	}
	return fmt.Sprintf("BootstrapReady is %s with %s: %s", c.Status, c.Reason, c.Message)
}

// setRecoveryFailedHAReady keeps HAReady False with the failure's reason
// (BootstrapReady's) once the recovery bootstrap of an HA cluster is Failed,
// as a failed HA bootstrap does (#770); msg says that the recovery failed.
func setRecoveryFailedHAReady(cluster *databasev1alpha1.CubridCluster, msg string) {
	if cluster.Status.Bootstrap.Phase != databasev1alpha1.BootstrapFailed || !cluster.Spec.HighAvailability.Enabled {
		return
	}
	reason := reasonBootstrapRecoveryFailed
	if c := meta.FindStatusCondition(cluster.Status.Conditions, conditionBootstrapReady); c != nil &&
		c.Status == metav1.ConditionFalse && c.Reason != "" {
		reason = c.Reason
	}
	setCondition(cluster, conditionHAReady, metav1.ConditionFalse, reason, msg)
}

// idleRetry is how soon a cluster that nothing else brings back is reconciled
// again, zero when it can wait for the next event.
func (r *CubridClusterReconciler) idleRetry(cluster *databasev1alpha1.CubridCluster) time.Duration {
	// A standalone member is not observed periodically (#155), but a member
	// whose engine version is not known yet is asked again: otherwise an idle
	// cluster stays blocked until an unrelated event.
	if r.standaloneVersionPending(cluster) {
		return standaloneVersionRetryInterval
	}
	// Removing a refused object, which has no reference to this cluster,
	// starts no reconcile.
	if report, ok := r.reports.Load(client.ObjectKeyFromObject(cluster)); ok && report.(*reconcileReport).brokerRefused {
		return refusedBrokerRetryInterval
	}
	return 0
}

// reasonPrimaryUnresolved is Ready's reason while the primary has stayed
// unresolved for longer than primaryUnresolvedGrace (#716).
const reasonPrimaryUnresolved = "PrimaryUnresolved"

// primaryUnresolvedGrace is how long PrimaryResolved may stay False before
// Ready follows it. A failover leaves the primary unresolved for a few
// seconds (5 to 13 s in docs/poc/RESULTS.md); in POC-51 it stayed unresolved
// for more than 8 minutes while the clients of -rw were refused.
const primaryUnresolvedGrace = 60 * time.Second

// setReady sets Ready. Every instance ready makes the cluster
// Ready, except while more than one master is observed: two masters are never
// reported as a healthy cluster (ADR-0001). A primary that is only unresolved,
// as during a failover or while a member is unreachable, leaves Ready to the
// instances for primaryUnresolvedGrace, counted from when PrimaryResolved was
// last True (unresolvedStart); HAReady and RoutingReady report it at once.
// Past that, Ready is False with PrimaryUnresolved (#716): this reports
// only, and the Operator still chooses no primary (ADR-0005).
//
// An HA bootstrap that stopped as Failed makes Ready False with the
// bootstrap's reason (#770), whether or not every instance is ready: it waits
// for a person and nothing is starting. An HA bootstrap still in progress that
// has not recorded a member as seeded keeps Ready False too: with
// InstancesNotReady while a member's server does not run yet, and with
// MemberNotSeeded once every instance is ready, as while the last member
// catches up after its seed restore. The order is: a write quarantine in
// force, which makes Ready False with its reason at once (ADR-0005, section
// 8), two masters observed on ready instances, a failed HA bootstrap, then on
// ready instances a member not seeded, a primary unresolved past the grace
// period, or else ClusterReady, and otherwise InstancesNotReady.
func (r *CubridClusterReconciler) setReady(cluster *databasev1alpha1.CubridCluster, ready, desired int32, multiplePrimaries bool) {
	wasReady := meta.IsStatusConditionTrue(cluster.Status.Conditions, conditionReady)
	instancesReady := ready >= desired && desired > 0
	unresolved := meta.FindStatusCondition(cluster.Status.Conditions, conditionPrimaryResolved)
	since, unresolvedNow := r.unresolvedStart(cluster, unresolved)
	unresolvedLong := unresolvedNow && r.now().Sub(since) >= primaryUnresolvedGrace
	bootReason, bootMessage, bootFailed := haBootstrapFailure(cluster)
	// Without a failure, what the bootstrap left unadmitted is a member not
	// yet seeded (MemberNotSeeded).
	_, unseededMessage, unseeded := haBootstrapUnadmitted(cluster)
	switch {
	case cluster.Status.WriteQuarantine != nil:
		// A quarantine of the read-write path makes Ready False at once,
		// without the grace of PrimaryUnresolved (ADR-0005, section 8).
		q := cluster.Status.WriteQuarantine
		setCondition(cluster, conditionReady, metav1.ConditionFalse, string(q.Reason),
			fmt.Sprintf("%d/%d instances ready, but %s", ready, desired, quarantineConditionMessage(q)))
		setCondition(cluster, conditionProgressing, metav1.ConditionFalse, string(q.Reason),
			"the read-write Brokers are kept stopped until the quarantine is released, see RoutingReady")
	case instancesReady && multiplePrimaries:
		setCondition(cluster, conditionReady, metav1.ConditionFalse, reasonMultiplePrimaries,
			fmt.Sprintf("%d/%d instances ready, but more than one member reports itself master", ready, desired))
		setCondition(cluster, conditionProgressing, metav1.ConditionFalse, "Reconciled",
			"cluster reconciled; the operator does not choose between masters, see PrimaryResolved")
	case bootFailed:
		setCondition(cluster, conditionReady, metav1.ConditionFalse, bootReason,
			fmt.Sprintf("%d/%d instances ready, but %s", ready, desired, bootMessage))
		setCondition(cluster, conditionProgressing, metav1.ConditionFalse, bootReason,
			"the HA bootstrap failed and waits for a person, see BootstrapReady")
	case instancesReady && unseeded:
		setCondition(cluster, conditionReady, metav1.ConditionFalse, reasonMemberNotSeeded,
			fmt.Sprintf("%d/%d instances ready, but %s", ready, desired, unseededMessage))
		setCondition(cluster, conditionProgressing, metav1.ConditionTrue, reasonMemberNotSeeded,
			"the HA bootstrap has not finished; "+bootstrapProgress(cluster))
	case instancesReady && unresolvedLong:
		setCondition(cluster, conditionReady, metav1.ConditionFalse, reasonPrimaryUnresolved,
			fmt.Sprintf("%d/%d instances ready, but no single primary is resolved since %s (%s)", ready, desired,
				since.UTC().Format(time.RFC3339), unresolved.Reason))
		setCondition(cluster, conditionProgressing, metav1.ConditionFalse, "Reconciled",
			"cluster reconciled; the operator does not choose a primary, see PrimaryResolved")
	case instancesReady:
		setCondition(cluster, conditionReady, metav1.ConditionTrue, "ClusterReady",
			fmt.Sprintf("%d/%d instances ready", ready, desired))
		setCondition(cluster, conditionProgressing, metav1.ConditionFalse, "Reconciled", "cluster reconciled")
		if !wasReady {
			r.statusEvent(cluster, corev1.EventTypeNormal, "ClusterReady",
				fmt.Sprintf("all %d instances ready", desired))
		}
	default:
		setCondition(cluster, conditionReady, metav1.ConditionFalse, "InstancesNotReady",
			fmt.Sprintf("%d/%d instances ready", ready, desired))
		setCondition(cluster, conditionProgressing, metav1.ConditionTrue, "InstancesStarting",
			fmt.Sprintf("waiting for %d/%d instances", ready, desired))
	}
}

// unresolvedStart says whether PrimaryResolved is False, and since when it has
// not been True. A pass that ends before the roles are observed sets it to
// Unknown, and the next observation sets it False with a new
// lastTransitionTime; the span goes on from the earlier start, so writes that
// fail more often than primaryUnresolvedGrace do not keep Ready True. Only a
// resolved primary ends the span. The start is kept in memory: after a restart
// of the Operator it is the lastTransitionTime again.
func (r *CubridClusterReconciler) unresolvedStart(cluster *databasev1alpha1.CubridCluster, unresolved *metav1.Condition) (time.Time, bool) {
	key := client.ObjectKeyFromObject(cluster)
	if unresolved == nil || unresolved.Status == metav1.ConditionTrue {
		r.unresolvedSince.Delete(key)
		return time.Time{}, false
	}
	since := unresolved.LastTransitionTime.Time
	if kept, ok := r.unresolvedSince.Load(key); ok && kept.(time.Time).Before(since) {
		since = kept.(time.Time)
	}
	r.unresolvedSince.Store(key, since)
	return since, unresolved.Status == metav1.ConditionFalse
}

// failed records a failure condition and returns the error for requeue. A
// write refused because the namespace is being deleted ends the reconcile
// with no Condition, Event or requeue. A conflict means the object was read
// before someone else changed it, which says nothing about the cluster: it is retried with the conditions and
// Events left as they are, unless the writes have kept conflicting for longer
// than conflictPatience.
func (r *CubridClusterReconciler) failed(ctx context.Context, cluster *databasev1alpha1.CubridCluster, reason string, cause error) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	if namespaceTerminating(cause) {
		// Nothing to retry and nothing to report: the cluster goes with its
		// namespace.
		log.V(1).Info("Stopped reconcile because the namespace is being deleted", "reason", reason)
		return ctrl.Result{}, nil
	}
	young, cause := r.lastingConflict(cluster, childWrites, cause)
	if young {
		log.V(1).Info("Retrying after a conflicting write", "reason", reason)
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	log.Error(cause, "Failed to write an object of the cluster", "reason", reason)
	// A failure that stays the same is retried many times and reported once.
	was := meta.FindStatusCondition(cluster.Status.Conditions, conditionReady)
	same := was != nil && was.Status == metav1.ConditionFalse && was.Reason == reason && was.Message == cause.Error()
	setCondition(cluster, conditionReady, metav1.ConditionFalse, reason, cause.Error())
	setCondition(cluster, conditionProgressing, metav1.ConditionTrue, reason, cause.Error())
	r.markRolesNotObserved(cluster, reason)
	if !same {
		r.statusEvent(cluster, corev1.EventTypeWarning, reason, cause.Error())
	}
	// Best-effort status update; return the original cause for requeue.
	_ = r.writeStatus(ctx, cluster)
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
		Owns(&policyv1.PodDisruptionBudget{}).
		Named("cubridcluster").
		WithOptions(controller.Options{RateLimiter: clusterRateLimiter()}).
		Complete(r)
}

// applierPositions is the store of applier readings this reconciler writes.
func (r *CubridClusterReconciler) applierPositions() *ApplierPositions {
	if r.Positions != nil {
		return r.Positions
	}
	return &r.ownPositions
}
