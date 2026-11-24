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
// others matching does not set the condition back to True, and it does not
// shorten the poll on its own.
func TestDBAPasswordRotationUnaskedMemberIsNoMatch(t *testing.T) {
	c, _ := dbaMembersCluster()
	c.UID = "pw-uid"
	c.Spec.DBAPasswordSecretRef = &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "pw-secret"}, Key: dbaTestKey}
	setCondition(c, conditionDBAPasswordSecretResolved, metav1.ConditionFalse, reasonDBAPasswordRotationUnsupported,
		"reported")
	checker := &fakeDBAPasswordChecker{}
	checker.set(instancemanager.DBAPasswordMatches, "", nil)
	const poll = 2 * time.Minute
	now := time.Now()
	r := &CubridClusterReconciler{Recorder: record.NewFakeRecorder(10), DBAPassword: checker, DBAPasswordPoll: poll,
		Clock: func() time.Time { return now }}

	if wait := r.reconcileDBAPasswordRotation(context.Background(), c, []string{pw0, pw1},
		[]string{pw2}); wait != poll {
		t.Errorf("next check in %s, want the poll %s", wait, poll)
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

// Only an answer of the primary (members[0]) that cannot tell is retried
// after dbaPasswordInconclusiveRetry (#1031). A slave that cannot tell, such
// as one whose Instance Manager predates #1012 and answers Unknown on every
// check, leaves the normal poll, so that it does not bring a login on every
// member every 30 seconds. A member that differs is reported whatever the
// others answer, also next to a call that failed.
func TestDBAPasswordRotationRetriesSoonOnlyForThePrimary(t *testing.T) {
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
			wantWait: poll, wantReason: reasonDBAPasswordRotationUnsupported},
		{name: "the call to a slave fails", primary: instancemanager.DBAPasswordMatches, slaveErr: true,
			other: instancemanager.DBAPasswordMatches, wantWait: poll, wantReason: reasonDBAPasswordRotationUnsupported},
		{name: "the primary answers Unknown", primary: instancemanager.DBAPasswordUnknown,
			slave: instancemanager.DBAPasswordMatches, other: instancemanager.DBAPasswordMatches,
			wantWait: dbaPasswordInconclusiveRetry, wantReason: reasonDBAPasswordRotationUnsupported},
		{name: "one slave differs and the call to the other fails", primary: instancemanager.DBAPasswordMatches,
			slaveErr: true, other: instancemanager.DBAPasswordDiffers, wantWait: poll,
			wantReason: reasonDBAPasswordRotationUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := dbaMembersCluster()
			c.UID = "pw-uid"
			c.Spec.DBAPasswordSecretRef = &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "pw-secret"}, Key: dbaTestKey}
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
