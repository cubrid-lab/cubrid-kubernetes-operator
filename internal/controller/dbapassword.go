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
	"strings"
	"sync"
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
// ready member of a standalone cluster, or every member of an HA cluster, is
// asked whether the value is still its DBA password
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
	// (instancemanager.ReadDBAPassword), CUBRID's own bound on a password
	// (docs/poc/RESULTS.md, POC-64).
	maxDBAPasswordBytes = 31
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
	case instancemanager.HasDBAPasswordControlByte(value):
		return invalid("holds an ASCII control byte")
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
		// only the members' answers clear that (reconcileDBAPasswordRotation).
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

// defaultDBAPasswordPoll is how often, by default, a member of a cluster with
// a DBA password Secret is asked whether the Secret's value is still the
// database's password. The operator does not watch the user's Secrets (#282),
// so this is how long a changed value may go unreported.
const defaultDBAPasswordPoll = 5 * time.Minute

// dbaPasswordInconclusiveRetry is how soon a member whose answer could not
// tell (Unknown, a failed call) is asked again, when that is sooner than the
// poll: such an answer says nothing about the password, so it does not hold
// back the next check for a whole poll (#1031).
const dbaPasswordInconclusiveRetry = 30 * time.Second

// dbaPasswordQuickRetries is how many checks in a row are asked after
// dbaPasswordInconclusiveRetry because a slave could not tell or was not
// asked; the checks after them wait for the poll again. A slave whose
// Instance Manager predates #1012, or one that stays in a transition, cannot
// tell on every check, and without a bound would bring a login on every
// member every dbaPasswordInconclusiveRetry for as long as it does. Three
// short retries cover a member that cannot tell for about a minute and a
// half, such as one whose heartbeat status command was killed while it was
// asked.
const dbaPasswordQuickRetries = 3

// dbaPasswordSchedule is what the reconciler's dbaPasswordNextCheck keeps for
// a cluster: when its members may next be asked, and how many short retries
// after a slave that could not tell or was not asked were made since the last
// check that settled (reconcileDBAPasswordRotation).
type dbaPasswordSchedule struct {
	at           time.Time
	quickRetries int
}

func (r *CubridClusterReconciler) dbaPasswordPoll() time.Duration {
	if r.DBAPasswordPoll > 0 {
		return r.DBAPasswordPoll
	}
	return defaultDBAPasswordPoll
}

// reconcileDBAPasswordRotation asks members, the members of a cluster with a
// DBA password Secret that may be asked now, whether the Secret's value is
// still each one's DBA password, and returns the delay after which to ask
// again; zero for a cluster without the reference. members[0] is the
// standalone member or the HA primary. unasked are the members
// that answer for the password too but may not be asked now, such as an HA
// slave that is not ready; each counts as an answer that cannot tell. members
// is empty when no member may be asked now (dbaPasswordStandaloneMember,
// dbaPasswordHAMembers): nothing is asked, the condition is left as it is,
// and the poll is returned.
//
// Each member keeps its own copy of the password: set_password changes only
// the member it runs on (docs/poc/RESULTS.md, POC-74), so a slave still on the
// old password would go unnoticed until a failover promoted it if only the
// primary were asked (#1012). So a match needs every member's answer:
//
//   - any member that answers Differs sets conditionDBAPasswordSecretResolved
//     False with reasonDBAPasswordRotationUnsupported, and records a Warning
//     Event when it was not already so. The message names the members that
//     refused the value and those that accepted it. One reason covers both a
//     Secret that no member accepts and members that disagree: the kubelet
//     writes a changed Secret into the members' files at different times, so
//     a mix of answers is also what a changed Secret looks like for a while;
//   - every member answering Matches sets it back to True;
//   - anything else (an Unknown answer, a failed call, a member not asked)
//     leaves the condition as it is, so that it does not flap, and is never
//     counted as a match.
//
// The new value is never applied, and neither the StatefulSet nor its Pods
// are changed. The members are asked at most once per poll per cluster, or
// sooner, after dbaPasswordInconclusiveRetry, when a check did not settle:
//
//   - after an Unknown answer or a failed call of members[0], on every such
//     check, as since #1031: the condition of a standalone cluster depends on
//     that one answer, and the primary is the member whose answer a client
//     sees;
//   - after a check in which no member answered Differs but a slave could not
//     tell or was not asked, at most dbaPasswordQuickRetries checks in a row;
//     the checks after them wait for the poll. A slave whose Instance Manager
//     is older than #1012, or one that stays in a transition, answers Unknown
//     on every check, and without the bound would bring a login on every
//     member every 30 seconds for as long as it does. The count starts again
//     after a check that settled: one in which a member answered Differs, or
//     every member was asked and answered Matches.
//
// Only the time of the next check and that count are kept, in memory, in the
// entry that already throttles the checks, so an operator that restarts asks
// again at once.
func (r *CubridClusterReconciler) reconcileDBAPasswordRotation(ctx context.Context,
	cluster *databasev1alpha1.CubridCluster, members, unasked []string) time.Duration {
	ref := cluster.Spec.DBAPasswordSecretRef
	if ref == nil {
		return 0
	}
	poll := r.dbaPasswordPoll()
	was := meta.FindStatusCondition(cluster.Status.Conditions, conditionDBAPasswordSecretResolved)
	// A Secret that does not resolve is reported as such, and is what has to
	// be corrected first.
	if r.DBAPassword == nil || len(members) == 0 || was == nil ||
		(was.Status != metav1.ConditionTrue && was.Reason != reasonDBAPasswordRotationUnsupported) {
		return poll
	}
	// The members are asked at most once per poll: each check is a login to
	// the database, and a cluster is reconciled on every event of its
	// objects. A next check further away than the poll is not trusted: the
	// clock went back.
	now := r.now()
	var quickRetries int
	if next, ok := r.dbaPasswordNextCheck.Load(cluster.UID); ok {
		next := next.(dbaPasswordSchedule)
		if wait := next.at.Sub(now); wait > 0 && wait <= poll {
			return wait
		}
		quickRetries = next.quickRetries
	}
	r.dbaPasswordNextCheck.Store(cluster.UID, dbaPasswordSchedule{at: now.Add(poll), quickRetries: quickRetries})
	log := logf.FromContext(ctx)
	// The members are asked at once, so that one that does not answer does
	// not hold back the others.
	checks := make([]instancemanager.DBAPasswordCheck, len(members))
	errs := make([]error, len(members))
	var wg sync.WaitGroup
	for i, member := range members {
		wg.Go(func() { checks[i], errs[i] = r.DBAPassword.CheckDBAPassword(ctx, member, cluster.Namespace) })
	}
	wg.Wait()
	var matched, differed []string
	primaryInconclusive := false
	for i, member := range members {
		switch {
		case errs[i] != nil:
			log.V(1).Info("Could not check the DBA password Secret against the database", "member", member,
				"error", errs[i].Error())
			primaryInconclusive = primaryInconclusive || i == 0
		case checks[i].State == instancemanager.DBAPasswordDiffers:
			differed = append(differed, member)
		case checks[i].State == instancemanager.DBAPasswordMatches:
			matched = append(matched, member)
		default:
			log.V(1).Info("Could not tell whether the DBA password Secret is the database's password", "member", member,
				"reason", checks[i].Reason)
			primaryInconclusive = primaryInconclusive || i == 0
		}
	}
	settled := len(differed) > 0 || (len(matched) == len(members) && len(unasked) == 0)
	switch {
	case len(differed) > 0:
		msg := fmt.Sprintf("the key %q of the Secret %s is not the database's current DBA password: the Secret's value "+
			"or the database's password was changed after it was set, and changing the DBA password is not supported. "+
			"The database keeps its password and the new value is not used; put the database's current DBA password "+
			"back into the Secret", ref.Key, ref.Name)
		msg += "; a dba login with it was refused on " + strings.Join(differed, ", ")
		if len(matched) > 0 {
			msg += " and accepted on " + strings.Join(matched, ", ") + ": the members' passwords differ, or the " +
				"kubelet has not yet written a changed Secret into every member's file. A password changed by hand " +
				"has to be changed on every member"
		}
		if was.Reason != reasonDBAPasswordRotationUnsupported {
			r.statusEvent(cluster, corev1.EventTypeWarning, reasonDBAPasswordRotationUnsupported,
				msg+"; the DB Pod is not restarted. A standalone member, created new or restored from a backup, "+
					"does not start its server after a restart until they match; an HA member is not checked when it "+
					"restarts, and this check goes on reporting the change")
		}
		setCondition(cluster, conditionDBAPasswordSecretResolved, metav1.ConditionFalse,
			reasonDBAPasswordRotationUnsupported, msg)
	case len(matched) == len(members) && len(unasked) == 0:
		if was.Reason == reasonDBAPasswordRotationUnsupported {
			log.Info("DBA password Secret holds the database's DBA password again", "secret", ref.Name)
			setCondition(cluster, conditionDBAPasswordSecretResolved, metav1.ConditionTrue,
				reasonDBAPasswordSecretResolved, dbaPasswordResolvedMessage(ref))
		}
	case len(unasked) > 0:
		log.V(1).Info("Could not ask every member whether the DBA password Secret is its password",
			"notAsked", strings.Join(unasked, ","))
	}
	// An answer that cannot tell said nothing about the password; the
	// members are asked again sooner than the poll: every time for the
	// standalone member or the primary (#1031), and a bounded number of
	// times in a row for a slave that cannot tell or was not asked.
	wait := poll
	if settled {
		quickRetries = 0
	}
	switch {
	case primaryInconclusive:
		wait = min(dbaPasswordInconclusiveRetry, poll)
	case !settled && quickRetries < dbaPasswordQuickRetries:
		quickRetries++
		wait = min(dbaPasswordInconclusiveRetry, poll)
	}
	r.dbaPasswordNextCheck.Store(cluster.UID, dbaPasswordSchedule{at: now.Add(wait), quickRetries: quickRetries})
	return wait
}

// dbaPasswordStandaloneMember is the member of a standalone cluster asked
// about its DBA password Secret: its one member, once it is ready.
func dbaPasswordStandaloneMember(cluster *databasev1alpha1.CubridCluster, sts *appsv1.StatefulSet) []string {
	if sts.Status.ReadyReplicas < 1 {
		return nil
	}
	return memberNames(cluster, 1)
}

// dbaPasswordHAMembers are the members of an HA cluster asked about its DBA
// password Secret, the primary first, and those that may not be asked now.
// Every member keeps
// its own copy of the password, which a password changed on one member does
// not reach (POC-74), so every member is asked (#1012): the primary that res
// resolved in this pass, which heartbeat reports as the master and whose role
// answer counts, and each member that is ready as a slave. A member that is
// not ready, or is in another role, is returned as not asked: it is not
// asked, and it keeps the cluster from being reported as a match. Its
// Instance Manager checks only while heartbeat reports its server as the
// active master or, on a slave, as standby (instancemanager checkDBAPassword).
//
// No member is asked while no primary is resolved, while more than one
// member claims to be the master, while the primary is not ready in the
// status, or while the HA bootstrap has not admitted every member or failed
// (haBootstrapUnadmitted), so a member that is still being seeded is never
// asked. A slave that lags is asked: it answers for its own password.
func dbaPasswordHAMembers(cluster *databasev1alpha1.CubridCluster, res PrimaryResolution) ([]string, []string) {
	if res.Status != metav1.ConditionTrue || res.CurrentPrimary == "" || res.MultipleMasters {
		return nil, nil
	}
	if _, _, unadmitted := haBootstrapUnadmitted(cluster); unadmitted {
		return nil, nil
	}
	instances := make(map[string]databasev1alpha1.InstanceStatus, len(cluster.Status.Instances))
	for _, in := range cluster.Status.Instances {
		instances[in.Name] = in
	}
	if in, ok := instances[res.CurrentPrimary]; !ok || !in.Ready || in.Role != databasev1alpha1.RoleMaster {
		return nil, nil
	}
	// The primary comes first: its answer that cannot tell is retried soon
	// every time, a slave's only a bounded number of times in a row
	// (reconcileDBAPasswordRotation).
	ask := []string{res.CurrentPrimary}
	var unasked []string
	for _, name := range memberNames(cluster, cluster.Spec.Topology.PromotableMembers) {
		in, ok := instances[name]
		switch {
		case name == res.CurrentPrimary:
		case ok && in.Ready && in.Role == databasev1alpha1.RoleSlave:
			ask = append(ask, name)
		default:
			unasked = append(unasked, name)
		}
	}
	return ask, unasked
}
