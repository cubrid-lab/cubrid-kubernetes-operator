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
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
)

// conditionDBAPasswordSecretResolved says whether the key that
// spec.dbaPasswordSecretRef names exists and holds a value the Instance
// Manager accepts. The DB container sets the password, or checks it, before
// its server starts, so a member that is ready already has it; afterwards the
// ready member is asked whether the value is still the database's password
// (reconcileDBAPasswordRotation). A cluster without the reference has no such
// condition.
const conditionDBAPasswordSecretResolved = "DBAPasswordSecretResolved"

// Reasons of conditionDBAPasswordSecretResolved. While the StatefulSet does
// not exist, Ready is False with the same reason as long as the key does not
// resolve.
const (
	reasonDBAPasswordSecretResolved   = "DBAPasswordSecretResolved"
	reasonDBAPasswordSecretNotFound   = "DBAPasswordSecretNotFound"
	reasonDBAPasswordKeyNotFound      = "DBAPasswordKeyNotFound"
	reasonDBAPasswordKeyEmpty         = "DBAPasswordKeyEmpty"
	reasonDBAPasswordValueInvalid     = "DBAPasswordValueInvalid"
	reasonDBAPasswordSecretReadFailed = "DBAPasswordSecretReadFailed"
)

// The DBA password reaches the DB container as a file of a Secret volume,
// which the kubelet writes; the operator never copies the value.
const (
	dbaPasswordVolume    = "dba-password"
	dbaPasswordMountPath = "/etc/cubrid-dba-password"
	dbaPasswordFileName  = "password"
	// dbaPasswordFileMode is the mode of the file. The kubelet writes a
	// Secret volume's files as root and, because the Pod sets fsGroup, gives
	// them the group cubridUID and adds the group read bit to the mode
	// anyway. So the cubrid user reads the file through its group: 0400
	// would be shown as 0440 on disk, and 0440 says so. No other user of the
	// container can read it.
	dbaPasswordFileMode int32 = 0o440
	// maxDBAPasswordBytes is the longest value the Instance Manager accepts
	// (instancemanager.ReadDBAPassword).
	maxDBAPasswordBytes = 256
)

// dbaPasswordFile is where the DB container finds the password.
const dbaPasswordFile = dbaPasswordMountPath + "/" + dbaPasswordFileName

// dbaPasswordVolumeFor is the Secret volume that holds only the referenced
// key, under dbaPasswordFileName. It is not optional: a Pod whose Secret or
// key is missing does not start, so no database is created without the
// password. No subPath is used, so the kubelet keeps the file up to date.
func dbaPasswordVolumeFor(ref *corev1.SecretKeySelector) corev1.Volume {
	mode := dbaPasswordFileMode
	return corev1.Volume{
		Name: dbaPasswordVolume,
		VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName:  ref.Name,
			Items:       []corev1.KeyToPath{{Key: ref.Key, Path: dbaPasswordFileName}},
			DefaultMode: &mode,
		}},
	}
}

// dbaPasswordProblem checks the key that spec.dbaPasswordSecretRef names in
// the cluster's namespace, by the same rules as the Instance Manager
// (instancemanager.ReadDBAPassword). It returns the reason and message of
// conditionDBAPasswordSecretResolved, and the error of a read that failed for
// another cause than a missing Secret. Neither ever holds the value.
//
// The Secret is read from the API server: the manager's cache holds only the
// Secrets the operator writes.
func (r *CubridClusterReconciler) dbaPasswordProblem(ctx context.Context,
	cluster *databasev1alpha1.CubridCluster) (string, string, error) {
	ref := cluster.Spec.DBAPasswordSecretRef
	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}
	secret := &corev1.Secret{}
	err := reader.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: ref.Name}, secret)
	switch {
	case apierrors.IsNotFound(err):
		return reasonDBAPasswordSecretNotFound,
			fmt.Sprintf("the Secret %s named by spec.dbaPasswordSecretRef does not exist", ref.Name), nil
	case err != nil:
		return reasonDBAPasswordSecretReadFailed,
			fmt.Sprintf("the operator could not read the Secret %s named by spec.dbaPasswordSecretRef: %v", ref.Name, err), err
	}
	value, ok := secret.Data[ref.Key]
	if !ok {
		return reasonDBAPasswordKeyNotFound,
			fmt.Sprintf("the Secret %s has no key %q, which spec.dbaPasswordSecretRef names", ref.Name, ref.Key), nil
	}
	// One line feed at the end is not part of the password.
	value = bytes.TrimSuffix(value, []byte("\n"))
	invalid := func(what string) (string, string, error) {
		return reasonDBAPasswordValueInvalid,
			fmt.Sprintf("the key %q of the Secret %s %s, which the Instance Manager refuses as a DBA password",
				ref.Key, ref.Name, what), nil
	}
	switch {
	case len(value) == 0:
		return reasonDBAPasswordKeyEmpty,
			fmt.Sprintf("the key %q of the Secret %s is empty", ref.Key, ref.Name), nil
	case len(value) > maxDBAPasswordBytes:
		return invalid(fmt.Sprintf("is longer than %d bytes", maxDBAPasswordBytes))
	case bytes.IndexByte(value, 0) >= 0:
		return invalid("holds a NUL byte")
	case bytes.IndexByte(value, '\r') >= 0:
		return invalid("holds a carriage return")
	case bytes.IndexByte(value, '\n') >= 0:
		return invalid("holds a line feed other than one at its end")
	}
	return reasonDBAPasswordSecretResolved, dbaPasswordResolvedMessage(ref), nil
}

// dbaPasswordResolvedMessage is the message of a resolved DBA password Secret.
func dbaPasswordResolvedMessage(ref *corev1.SecretKeySelector) string {
	return fmt.Sprintf("the key %q of the Secret %s holds a DBA password the DB container can read", ref.Key, ref.Name)
}

// reconcileDBAPasswordSecret reports whether the DBA password the cluster
// names resolves, in conditionDBAPasswordSecretResolved and, for a new
// problem, in a Warning Event. It returns true when the reconcile must stop
// here: while the StatefulSet does not exist, it is not created until the key
// resolves, because the database would be created without the password it
// asks for. Ready then says why. An existing StatefulSet is left as it is: its
// running members keep the file they mounted.
//
// While it waits, a missing Secret, a missing key or an unusable value ends
// the reconcile without an error and with the delay after which it is checked
// again (DBAPasswordRecheck): the operator does not watch the user's Secrets
// (#282). A read that failed for another cause is returned, and retried with
// backoff.
func (r *CubridClusterReconciler) reconcileDBAPasswordSecret(ctx context.Context,
	cluster *databasev1alpha1.CubridCluster) (bool, time.Duration, error) {
	if cluster.Spec.DBAPasswordSecretRef == nil {
		return false, 0, nil
	}
	reason, msg, readErr := r.dbaPasswordProblem(ctx, cluster)
	was := meta.FindStatusCondition(cluster.Status.Conditions, conditionDBAPasswordSecretResolved)
	if reason == reasonDBAPasswordSecretResolved {
		// A value that resolves may still not be the database's password;
		// only the member's answer clears that (reconcileDBAPasswordRotation).
		if was == nil || was.Reason != reasonDBAPasswordRotationUnsupported {
			setCondition(cluster, conditionDBAPasswordSecretResolved, metav1.ConditionTrue, reason, msg)
		}
		return false, 0, nil
	}
	same := was != nil && was.Status == metav1.ConditionFalse && was.Reason == reason && was.Message == msg
	setCondition(cluster, conditionDBAPasswordSecretResolved, metav1.ConditionFalse, reason, msg)

	err := r.childClient().Get(ctx, client.ObjectKeyFromObject(cluster), &appsv1.StatefulSet{})
	if err == nil {
		if !same {
			r.statusEvent(cluster, corev1.EventTypeWarning, reason,
				msg+"; the StatefulSet is left as it is, but a DB Pod that is started again does not start until this is corrected")
		}
		return false, 0, nil
	}
	if !apierrors.IsNotFound(err) {
		return true, 0, err
	}
	if !same {
		r.statusEvent(cluster, corev1.EventTypeWarning, reason,
			msg+"; the database is not created until it holds the DBA password")
	}
	logf.FromContext(ctx).V(1).Info("Waiting for the DBA password Secret before creating the StatefulSet",
		"secret", cluster.Spec.DBAPasswordSecretRef.Name, "reason", reason)
	setCondition(cluster, conditionReady, metav1.ConditionFalse, reason, msg+"; the database is not created yet")
	setCondition(cluster, conditionProgressing, metav1.ConditionFalse, reason,
		"waiting for the DBA password Secret named by spec.dbaPasswordSecretRef")
	if err := r.writeStatus(ctx, cluster); err != nil {
		return true, 0, err
	}
	if readErr != nil {
		return true, 0, readErr
	}
	return true, r.dbaPasswordRecheck(), nil
}

// defaultDBAPasswordRecheck is how long a cluster whose DBA password Secret
// does not resolve waits, by default, before it is checked again.
const defaultDBAPasswordRecheck = 30 * time.Second

func (r *CubridClusterReconciler) dbaPasswordRecheck() time.Duration {
	if r.DBAPasswordRecheck > 0 {
		return r.DBAPasswordRecheck
	}
	return defaultDBAPasswordRecheck
}

// dbaPasswordEnv is the Instance Manager's variable that names the password
// file (instancemanager.DBAPasswordFileEnv).
func dbaPasswordEnv() corev1.EnvVar {
	return corev1.EnvVar{Name: instancemanager.DBAPasswordFileEnv, Value: dbaPasswordFile}
}

// DBAPasswordChecker asks a member's Instance Manager whether the value of
// its mounted DBA password file is the running database's DBA password (GET
// /v1/dba-password). It is an interface so the reconciler can be tested
// without a live manager.
type DBAPasswordChecker interface {
	CheckDBAPassword(ctx context.Context, podName, namespace string) (instancemanager.DBAPasswordCheck, error)
}

// reasonDBAPasswordRotationUnsupported is the reason of
// conditionDBAPasswordSecretResolved while the Secret's value is not the
// database's DBA password: it was changed after the password was set, or the
// password was changed in the database. The database keeps its password.
const reasonDBAPasswordRotationUnsupported = "DBAPasswordRotationUnsupported"

// defaultDBAPasswordPoll is how often, by default, the member of a cluster
// with a DBA password Secret is asked whether the Secret's value is still the
// database's password. The operator does not watch the user's Secrets (#282),
// so this is how long a changed value may go unreported.
const defaultDBAPasswordPoll = 5 * time.Minute

func (r *CubridClusterReconciler) dbaPasswordPoll() time.Duration {
	if r.DBAPasswordPoll > 0 {
		return r.DBAPasswordPoll
	}
	return defaultDBAPasswordPoll
}

// reconcileDBAPasswordRotation asks the member of a standalone cluster with a
// DBA password Secret whether the Secret's value is still the database's DBA
// password, and returns the delay after which to ask again; zero for a
// cluster without the reference.
//
// Differs sets conditionDBAPasswordSecretResolved False with
// reasonDBAPasswordRotationUnsupported, and records a Warning Event when it
// was not already so. Matches sets it back to True. The new value is never
// applied, and neither the StatefulSet nor its Pod is changed. An answer that
// cannot tell (Unknown, a failed call, no member ready) leaves the condition
// as it is, so that it does not flap. The member is asked at most once per
// poll; only the time of the last check is kept, in memory, so an operator
// that restarts asks again at once.
func (r *CubridClusterReconciler) reconcileDBAPasswordRotation(ctx context.Context,
	cluster *databasev1alpha1.CubridCluster, sts *appsv1.StatefulSet) time.Duration {
	ref := cluster.Spec.DBAPasswordSecretRef
	if ref == nil || cluster.Spec.HighAvailability.Enabled {
		return 0
	}
	poll := r.dbaPasswordPoll()
	was := meta.FindStatusCondition(cluster.Status.Conditions, conditionDBAPasswordSecretResolved)
	// A Secret that does not resolve is reported as such, and is what has to
	// be corrected first.
	if r.DBAPassword == nil || sts.Status.ReadyReplicas < 1 || was == nil ||
		(was.Status != metav1.ConditionTrue && was.Reason != reasonDBAPasswordRotationUnsupported) {
		return poll
	}
	// The member is asked at most once per poll: each check is a login to the
	// database, and a cluster is reconciled on every event of its objects.
	now := r.now()
	if last, ok := r.dbaPasswordCheckedAt.Load(cluster.UID); ok {
		if since := now.Sub(last.(time.Time)); since >= 0 && since < poll {
			return poll - since
		}
	}
	r.dbaPasswordCheckedAt.Store(cluster.UID, now)
	log := logf.FromContext(ctx)
	member := memberNames(cluster, 1)[0]
	check, err := r.DBAPassword.CheckDBAPassword(ctx, member, cluster.Namespace)
	switch {
	case err != nil:
		log.V(1).Info("Could not check the DBA password Secret against the database", "member", member,
			"error", err.Error())
	case check.State == instancemanager.DBAPasswordDiffers:
		msg := fmt.Sprintf("the key %q of the Secret %s is not the database's current DBA password: the Secret's value "+
			"or the database's password was changed after it was set, and changing the DBA password is not supported. "+
			"The database keeps its password and the new value is not used; put the database's current DBA password "+
			"back into the Secret", ref.Key, ref.Name)
		if was.Reason != reasonDBAPasswordRotationUnsupported {
			r.statusEvent(cluster, corev1.EventTypeWarning, reasonDBAPasswordRotationUnsupported,
				msg+"; the DB Pod is not restarted, and a DB container that starts again does not start its server until they match")
		}
		setCondition(cluster, conditionDBAPasswordSecretResolved, metav1.ConditionFalse,
			reasonDBAPasswordRotationUnsupported, msg)
	case check.State == instancemanager.DBAPasswordMatches:
		if was.Reason == reasonDBAPasswordRotationUnsupported {
			log.Info("DBA password Secret holds the database's DBA password again", "secret", ref.Name)
			setCondition(cluster, conditionDBAPasswordSecretResolved, metav1.ConditionTrue,
				reasonDBAPasswordSecretResolved, dbaPasswordResolvedMessage(ref))
		}
	default:
		log.V(1).Info("Could not tell whether the DBA password Secret is the database's password", "member", member,
			"reason", check.Reason)
	}
	return poll
}
