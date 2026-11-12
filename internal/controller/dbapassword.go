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
// Manager accepts. True says nothing about the database: the DB container
// sets the password, or checks it, before its server starts, so a member that
// is ready already has it. A cluster without the reference has no such
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
	return reasonDBAPasswordSecretResolved,
		fmt.Sprintf("the key %q of the Secret %s holds a DBA password the DB container can read", ref.Key, ref.Name), nil
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
	if reason == reasonDBAPasswordSecretResolved {
		setCondition(cluster, conditionDBAPasswordSecretResolved, metav1.ConditionTrue, reason, msg)
		return false, 0, nil
	}
	was := meta.FindStatusCondition(cluster.Status.Conditions, conditionDBAPasswordSecretResolved)
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
