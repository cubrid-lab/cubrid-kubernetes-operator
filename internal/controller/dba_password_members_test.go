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
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
)

// The members of dbaMembersCluster.
const (
	pw0 = "pw-0"
	pw1 = "pw-1"
	pw2 = "pw-2"

	// pwUID and pwSecret are the UID of dbaMembersCluster and the name of its
	// DBA password Secret in the rotation tests.
	pwUID    = "pw-uid"
	pwSecret = "pw-secret"
)

// dbaMembersCluster is a three-member HA cluster whose bootstrap admitted
// every member, with pw-1 as the ready master and the others ready slaves,
// and its resolution.
func dbaMembersCluster() (*databasev1alpha1.CubridCluster, PrimaryResolution) {
	c := haCluster("pw")
	c.Status.Databases = []databasev1alpha1.DatabaseStatus{{Name: testDatabase, PrimaryCreated: true, HAConfigured: true}}
	c.Status.Instances = []databasev1alpha1.InstanceStatus{
		{Name: pw0, Role: databasev1alpha1.RoleSlave, Ready: true},
		{Name: pw1, Role: databasev1alpha1.RoleMaster, Ready: true},
		{Name: pw2, Role: databasev1alpha1.RoleSlave, Ready: true},
	}
	return c, PrimaryResolution{Status: metav1.ConditionTrue, CurrentPrimary: pw1}
}

// Every member of an HA cluster is asked, not only the primary: a password
// changed with set_password on one member does not reach the others
// (POC-74, #1012). A member that may not be asked now is returned as such, so
// that it keeps the cluster from being reported as a match.
func TestDBAPasswordHAMembers(t *testing.T) {
	c, res := dbaMembersCluster()
	if _, _, unadmitted := haBootstrapUnadmitted(c); unadmitted {
		t.Fatalf("the test cluster's bootstrap is not admitted: %+v", c.Status.Databases)
	}
	ask, unasked := dbaPasswordHAMembers(c, res)
	if !slices.Equal(ask, []string{pw1, pw0, pw2}) || len(unasked) != 0 {
		t.Fatalf("ask %q, not asked %q; want every member asked, the primary first", ask, unasked)
	}

	c.Status.Instances[2].Ready, c.Status.Instances[2].Role = false, databasev1alpha1.RoleUnknown
	ask, unasked = dbaPasswordHAMembers(c, res)
	if !slices.Equal(ask, []string{pw1, pw0}) || !slices.Equal(unasked, []string{pw2}) {
		t.Fatalf("ask %q, not asked %q; want pw-2 not asked", ask, unasked)
	}

	c.Status.Instances = c.Status.Instances[:2]
	_, unasked = dbaPasswordHAMembers(c, res)
	if !slices.Equal(unasked, []string{pw2}) {
		t.Fatalf("not asked %q; a member missing from the status must not be skipped", unasked)
	}

	c.Status.Instances[1].Ready = false
	if ask, unasked = dbaPasswordHAMembers(c, res); ask != nil || unasked != nil {
		t.Fatalf("ask %q, not asked %q; want nobody asked while the primary is not ready", ask, unasked)
	}
}

// A member that is not asked counts as an answer that cannot tell: the
// others matching does not set the condition back to True, and the members
// are asked again soon, as after a slave that cannot tell.
func TestDBAPasswordRotationUnaskedMemberIsNoMatch(t *testing.T) {
	c, _ := dbaMembersCluster()
	c.UID = pwUID
	c.Spec.DBAPasswordSecretRef = &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: pwSecret}, Key: dbaTestKey}
	setCondition(c, conditionDBAPasswordSecretResolved, metav1.ConditionFalse, reasonDBAPasswordRotationUnsupported,
		"reported")
	checker := &fakeDBAPasswordChecker{}
	checker.set(instancemanager.DBAPasswordMatches, "", nil)
	const poll = 2 * time.Minute
	now := time.Now()
	r := &CubridClusterReconciler{Recorder: record.NewFakeRecorder(10), DBAPassword: checker, DBAPasswordPoll: poll,
		Clock: func() time.Time { return now }}

	if wait := r.reconcileDBAPasswordRotation(context.Background(), c, []string{pw0, pw1},
		[]string{pw2}); wait != dbaPasswordInconclusiveRetry {
		t.Errorf("next check in %s, want the short retry %s", wait, dbaPasswordInconclusiveRetry)
	}
	got := meta.FindStatusCondition(c.Status.Conditions, conditionDBAPasswordSecretResolved)
	if got.Status != metav1.ConditionFalse || got.Reason != reasonDBAPasswordRotationUnsupported {
		t.Fatalf("condition %s/%s, want it kept False while pw-2 was not asked", got.Status, got.Reason)
	}

	now = now.Add(poll)
	r.reconcileDBAPasswordRotation(context.Background(), c, []string{pw0, pw1, pw2}, nil)
	got = meta.FindStatusCondition(c.Status.Conditions, conditionDBAPasswordSecretResolved)
	if got.Status != metav1.ConditionTrue || got.Reason != reasonDBAPasswordSecretResolved {
		t.Fatalf("condition %s/%s, want True once every member matches", got.Status, got.Reason)
	}
}

// An answer that cannot tell, of the primary (#1031) or of a slave, is
// retried after dbaPasswordInconclusiveRetry rather than after the poll: in a
// Kind e2e run the Secret was put back, two members answered Matches, and
// the condition stayed False for minutes. A member that differs settles the
// check: it is reported whatever the others answer, also next to a call that
// failed, and the members wait for the poll.
func TestDBAPasswordRotationRetriesSoonAfterAnAnswerThatCannotTell(t *testing.T) {
	const poll = 2 * time.Minute
	for _, tc := range []struct {
		name       string
		primary    instancemanager.DBAPasswordState
		slaveErr   bool
		slave      instancemanager.DBAPasswordState
		other      instancemanager.DBAPasswordState
		wantWait   time.Duration
		wantReason string
	}{
		{name: "a slave of an older Instance Manager answers Unknown", primary: instancemanager.DBAPasswordMatches,
			slave: instancemanager.DBAPasswordUnknown, other: instancemanager.DBAPasswordMatches,
			wantWait: dbaPasswordInconclusiveRetry, wantReason: reasonDBAPasswordRotationUnsupported},
		{name: "the call to a slave fails", primary: instancemanager.DBAPasswordMatches, slaveErr: true,
			other: instancemanager.DBAPasswordMatches, wantWait: dbaPasswordInconclusiveRetry,
			wantReason: reasonDBAPasswordRotationUnsupported},
		{name: "the primary answers Unknown", primary: instancemanager.DBAPasswordUnknown,
			slave: instancemanager.DBAPasswordMatches, other: instancemanager.DBAPasswordMatches,
			wantWait: dbaPasswordInconclusiveRetry, wantReason: reasonDBAPasswordRotationUnsupported},
		{name: "one slave differs and the call to the other fails", primary: instancemanager.DBAPasswordMatches,
			slaveErr: true, other: instancemanager.DBAPasswordDiffers, wantWait: poll,
			wantReason: reasonDBAPasswordRotationUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := dbaMembersCluster()
			c.UID = pwUID
			c.Spec.DBAPasswordSecretRef = &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: pwSecret}, Key: dbaTestKey}
			// The condition starts True for the case of a member that differs,
			// and reported otherwise, so that a wrong match would show.
			start := tc.other != instancemanager.DBAPasswordDiffers
			if start {
				setCondition(c, conditionDBAPasswordSecretResolved, metav1.ConditionFalse,
					reasonDBAPasswordRotationUnsupported, "reported")
			} else {
				setCondition(c, conditionDBAPasswordSecretResolved, metav1.ConditionTrue,
					reasonDBAPasswordSecretResolved, "resolved")
			}
			checker := &failingSlaveChecker{fakeDBAPasswordChecker: &fakeDBAPasswordChecker{}}
			checker.setMember(pw1, tc.primary)
			checker.setMember(pw2, tc.other)
			if tc.slaveErr {
				checker.failing = pw0
			} else {
				checker.setMember(pw0, tc.slave)
			}
			recorder := record.NewFakeRecorder(10)
			now := time.Now()
			r := &CubridClusterReconciler{Recorder: recorder, DBAPassword: checker, DBAPasswordPoll: poll,
				Clock: func() time.Time { return now }}

			if wait := r.reconcileDBAPasswordRotation(context.Background(), c, []string{pw1, pw0, pw2},
				nil); wait != tc.wantWait {
				t.Errorf("next check in %s, want %s", wait, tc.wantWait)
			}
			got := meta.FindStatusCondition(c.Status.Conditions, conditionDBAPasswordSecretResolved)
			if got.Status != metav1.ConditionFalse || got.Reason != tc.wantReason {
				t.Fatalf("condition %s/%s, want False/%s", got.Status, got.Reason, tc.wantReason)
			}
			if !start {
				if !strings.Contains(got.Message, "refused on "+pw2+" and accepted on "+pw1) {
					t.Errorf("message %q does not name the member that refused and the one that accepted", got.Message)
				}
				if len(recorder.Events) != 1 {
					t.Errorf("%d Events, want one Warning Event", len(recorder.Events))
				}
			}
		})
	}
}

// A slave that cannot tell brings at most dbaPasswordQuickRetries short
// retries in a row, then the poll, so that a slave whose Instance Manager
// predates #1012 and answers Unknown on every check does not bring a login on
// every member every 30 seconds forever. The count starts again after a
// check that settled, and an answer of the primary that cannot tell is
// retried soon every time, as before.
func TestDBAPasswordRotationBoundsTheShortRetriesOfASlave(t *testing.T) {
	const poll = 2 * time.Minute
	c, _ := dbaMembersCluster()
	c.UID = pwUID
	c.Spec.DBAPasswordSecretRef = &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: pwSecret}, Key: dbaTestKey}
	setCondition(c, conditionDBAPasswordSecretResolved, metav1.ConditionTrue, reasonDBAPasswordSecretResolved,
		"resolved")
	checker := &fakeDBAPasswordChecker{}
	checker.set(instancemanager.DBAPasswordMatches, "", nil)
	now := time.Now()
	newReconciler := func() *CubridClusterReconciler {
		return &CubridClusterReconciler{Recorder: record.NewFakeRecorder(100), DBAPassword: checker,
			DBAPasswordPoll: poll, Clock: func() time.Time { return now }}
	}
	r := newReconciler()
	members := []string{pw1, pw0, pw2}
	// check asks the members after d, and returns the delay to the next check.
	check := func(d time.Duration) time.Duration {
		t.Helper()
		now = now.Add(d)
		asked := len(checker.calls())
		wait := r.reconcileDBAPasswordRotation(context.Background(), c, members, nil)
		if got := len(checker.calls()) - asked; got != len(members) {
			t.Fatalf("%d members asked, want %d", got, len(members))
		}
		return wait
	}
	reason := func() string {
		return meta.FindStatusCondition(c.Status.Conditions, conditionDBAPasswordSecretResolved).Reason
	}

	// The Secret is changed: every member refuses it.
	checker.set(instancemanager.DBAPasswordDiffers, "refused", nil)
	if wait := check(0); wait != poll || reason() != reasonDBAPasswordRotationUnsupported {
		t.Fatalf("next check in %s, reason %s; want the poll and the change reported", wait, reason())
	}

	// The Secret is put back; the primary and one slave match, and the other
	// slave cannot tell, as in the Kind e2e run.
	checker.set(instancemanager.DBAPasswordMatches, "", nil)
	checker.setMember(pw2, instancemanager.DBAPasswordUnknown)
	if wait := check(poll); wait != dbaPasswordInconclusiveRetry {
		t.Fatalf("next check in %s after a slave that cannot tell, want %s", wait, dbaPasswordInconclusiveRetry)
	}
	if reason() != reasonDBAPasswordRotationUnsupported {
		t.Fatalf("reason %s, want the change kept reported while a slave cannot tell", reason())
	}
	checker.setMember(pw2, "")
	if wait := check(dbaPasswordInconclusiveRetry); wait != poll || reason() != reasonDBAPasswordSecretResolved {
		t.Fatalf("next check in %s, reason %s; want the poll and True once every member matches", wait, reason())
	}

	// A slave that cannot tell on every check: exactly dbaPasswordQuickRetries
	// short retries, then the poll, also on the checks after it.
	checker.setMember(pw2, instancemanager.DBAPasswordUnknown)
	// slaveCannotTell checks first after first.
	slaveCannotTell := func(first time.Duration) {
		t.Helper()
		wait := check(first)
		for i := range dbaPasswordQuickRetries {
			if wait != dbaPasswordInconclusiveRetry {
				t.Fatalf("check %d: next check in %s, want the short retry %s", i+1, wait,
					dbaPasswordInconclusiveRetry)
			}
			wait = check(wait)
		}
		for range 2 {
			if wait != poll {
				t.Fatalf("next check in %s after %d short retries, want the poll %s", wait,
					dbaPasswordQuickRetries, poll)
			}
			wait = check(wait)
		}
	}
	slaveCannotTell(poll)

	// A check that settles starts the count again: every member matching...
	checker.setMember(pw2, "")
	if wait := check(poll); wait != poll {
		t.Fatalf("next check in %s once every member matches, want the poll", wait)
	}
	checker.setMember(pw2, instancemanager.DBAPasswordUnknown)
	slaveCannotTell(poll)

	// ...or a member that refuses the value, even while the other slave still
	// cannot tell.
	checker.setMember(pw0, instancemanager.DBAPasswordDiffers)
	if wait := check(poll); wait != poll || reason() != reasonDBAPasswordRotationUnsupported {
		t.Fatalf("next check in %s, reason %s; want the poll and the change reported", wait, reason())
	}
	checker.setMember(pw0, "")
	slaveCannotTell(poll)

	// An operator that restarts keeps nothing: it asks at once, and starts
	// the count again.
	r = newReconciler()
	slaveCannotTell(0)

	// The primary that cannot tell is retried soon on every check, however
	// many short retries were made.
	checker.setMember(pw1, instancemanager.DBAPasswordUnknown)
	wait := check(poll)
	for range 2 * dbaPasswordQuickRetries {
		if wait != dbaPasswordInconclusiveRetry {
			t.Fatalf("next check in %s while the primary cannot tell, want %s", wait, dbaPasswordInconclusiveRetry)
		}
		wait = check(wait)
	}
}

// failingSlaveChecker is fakeDBAPasswordChecker whose call to failing fails.
type failingSlaveChecker struct {
	*fakeDBAPasswordChecker
	failing string
}

func (f *failingSlaveChecker) CheckDBAPassword(ctx context.Context, podName, namespace string) (instancemanager.DBAPasswordCheck, error) {
	if podName == f.failing {
		return instancemanager.DBAPasswordCheck{}, errors.New("connection refused")
	}
	return f.fakeDBAPasswordChecker.CheckDBAPassword(ctx, podName, namespace)
}
