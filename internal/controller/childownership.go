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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// childNotOwnedError is the refusal of an object that has a name the cluster
// needs and that the cluster does not control.
type childNotOwnedError struct {
	kind, name string
	// controller is the object's controller owner, nil when it has none.
	controller *metav1.OwnerReference
}

func (e *childNotOwnedError) Error() string {
	msg := fmt.Sprintf("a %s named %q exists and is not owned by this cluster", e.kind, e.name)
	if e.controller != nil {
		// The UID tells a cluster that was deleted from this one of the same name.
		msg += fmt.Sprintf(" (its controller is %s %s, UID %s)", e.controller.Kind, e.controller.Name, e.controller.UID)
	}
	return msg + "; it was left unchanged, and the cluster cannot use the name until it is free"
}

// cacheMissReads is a client that asks the API server for an object the
// manager's cache does not hold. The cache holds only objects with the
// managed-by label, so a miss says nothing about a name: it may be taken by an
// object of someone else, or by the cluster's own object whose label was
// removed or that the cache has not caught up with.
type cacheMissReads struct {
	client.Client
	apiServer client.Reader
}

func (c cacheMissReads) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	err := c.Client.Get(ctx, key, obj, opts...)
	if apierrors.IsNotFound(err) {
		return c.apiServer.Get(ctx, key, obj, opts...)
	}
	return err
}

// childClient reads a cluster's child objects by name whether or not the
// cache holds them.
func (r *CubridClusterReconciler) childClient() client.Client {
	if r.APIReader == nil {
		return r.Client
	}
	return cacheMissReads{Client: r.Client, apiServer: r.APIReader}
}

// reconcileChild creates the object, or updates it if this cluster controls
// it, with what mutate sets, and makes the cluster its controller. An object of
// that name which the cluster does not control is someone else's, whatever its
// name, labels or other owners say, and is refused with a childNotOwnedError
// before anything is written. Every operator version has created its objects
// with the controller reference, so an object the cluster's own operator made
// is never refused.
//
// The check is made on the object the write rests on, inside CreateOrUpdate,
// so no second read leaves a gap: a read that finds nothing ends in a create,
// which the API server refuses if the name was taken meanwhile, and an update
// carries the resource version that was checked, which the API server refuses
// if the object changed meanwhile.
//
// An object the cache does not hold is checked in the same way, so that an
// object of someone else is refused rather than reported as AlreadyExists on
// every reconcile, and the cluster's own object gets its label back and
// returns to the cache. The token Secret is the exception: an unlabeled Secret
// is not read as a child, and imTokenSecretRefused explains the refused create.
func (r *CubridClusterReconciler) reconcileChild(ctx context.Context, cluster *databasev1alpha1.CubridCluster,
	obj client.Object, mutate func() error) error {
	writer := r.childClient()
	if _, secret := obj.(*corev1.Secret); secret {
		writer = r.Client
	}
	_, err := controllerutil.CreateOrUpdate(ctx, writer, obj, func() error {
		if obj.GetResourceVersion() != "" && !metav1.IsControlledBy(obj, cluster) {
			gvk, err := apiutil.GVKForObject(obj, r.Scheme)
			if err != nil {
				return err
			}
			return &childNotOwnedError{kind: gvk.Kind, name: obj.GetName(), controller: metav1.GetControllerOf(obj)}
		}
		if err := mutate(); err != nil {
			return err
		}
		return controllerutil.SetControllerReference(cluster, obj, r.Scheme)
	})
	return err
}
