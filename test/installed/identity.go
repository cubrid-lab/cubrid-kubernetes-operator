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

package installed

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

type imageIdentity struct {
	Pod         string `json:"pod"`
	UID         string `json:"uid"`
	Node        string `json:"node"`
	Image       string `json:"image"`
	ImageID     string `json:"imageID"`
	ContainerID string `json:"containerID"`
}

type managerIdentity struct {
	DeploymentUID string          `json:"deploymentUID"`
	Pods          []imageIdentity `json:"pods"`
}

func controllerOwner(obj metav1.Object, kind string, uid types.UID) bool {
	for _, owner := range obj.GetOwnerReferences() {
		if owner.Controller != nil && *owner.Controller && owner.Kind == kind && owner.UID == uid && uid != "" {
			return true
		}
	}
	return false
}

func podIdentity(pod *corev1.Pod, container, expected string) (imageIdentity, error) {
	identity := imageIdentity{Pod: pod.Name, UID: string(pod.UID), Node: pod.Spec.NodeName}
	if pod.UID == "" || pod.Spec.NodeName == "" || !pod.DeletionTimestamp.IsZero() || !podReady(pod) {
		return identity, errors.New("a ready non-terminating Pod identity was not observed")
	}
	for _, c := range pod.Spec.Containers {
		if c.Name == container {
			identity.Image = c.Image
		}
	}
	if identity.Image != expected {
		return identity, fmt.Errorf("pod %s does not declare the expected %s image", pod.Name, container)
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == container {
			if !status.Ready || status.State.Running == nil {
				return identity, fmt.Errorf("pod %s has no running, ready %s container", pod.Name, container)
			}
			identity.ImageID = status.ImageID
			identity.ContainerID = status.ContainerID
		}
	}
	if identity.ImageID == "" || identity.ContainerID == "" {
		return identity, fmt.Errorf("pod %s has no observed runtime identity for %s", pod.Name, container)
	}
	return identity, nil
}

func podReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func (r *Runner) observeManager(ctx context.Context, cfg Config) (managerIdentity, error) {
	var identity managerIdentity
	deployment := &appsv1.Deployment{}
	key := client.ObjectKey{Namespace: cfg.OperatorNamespace, Name: cfg.OperatorDeployment}
	if err := r.Client.Get(ctx, key, deployment); err != nil {
		return identity, fmt.Errorf("observe installed manager: %w", err)
	}
	identity.DeploymentUID = string(deployment.UID)
	desired := int32(1)
	if deployment.Spec.Replicas != nil {
		desired = *deployment.Spec.Replicas
	}
	if deployment.UID == "" || desired < 1 || deployment.Status.ObservedGeneration < deployment.Generation ||
		deployment.Status.Replicas != desired || deployment.Status.UpdatedReplicas != desired ||
		deployment.Status.ReadyReplicas != desired || deployment.Status.AvailableReplicas != desired {
		return identity, errors.New("installed manager Deployment is not stable and available")
	}
	if deployment.Spec.Selector == nil {
		return identity, errors.New("installed manager has no Pod selector")
	}
	declared := ""
	for _, container := range deployment.Spec.Template.Spec.Containers {
		if container.Name == "manager" {
			declared = container.Image
		}
	}
	if declared != cfg.OperatorImage {
		return identity, errors.New("installed manager Deployment does not declare the expected image")
	}
	selector, err := metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
	if err != nil {
		return identity, err
	}
	pods := &corev1.PodList{}
	if err := r.Client.List(ctx, pods, client.InNamespace(cfg.OperatorNamespace),
		client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return identity, err
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !pod.DeletionTimestamp.IsZero() {
			continue
		}
		for _, owner := range pod.OwnerReferences {
			if owner.Kind != "ReplicaSet" || owner.Controller == nil || !*owner.Controller {
				continue
			}
			rs := &appsv1.ReplicaSet{}
			if err := r.Client.Get(ctx, client.ObjectKey{Namespace: cfg.OperatorNamespace, Name: owner.Name}, rs); err != nil {
				return identity, err
			}
			if rs.UID != owner.UID || !controllerOwner(rs, "Deployment", deployment.UID) {
				continue
			}
			observed, err := podIdentity(pod, "manager", cfg.OperatorImage)
			if err != nil {
				return identity, err
			}
			identity.Pods = append(identity.Pods, observed)
		}
	}
	if len(identity.Pods) != int(desired) {
		return identity, errors.New("expected manager Pods were not verified through Deployment and ReplicaSet ownership")
	}
	slices.SortFunc(identity.Pods, func(a, b imageIdentity) int { return cmp.Compare(a.Pod, b.Pod) })
	for _, pod := range identity.Pods {
		if pod.ImageID != identity.Pods[0].ImageID {
			return identity, errors.New("installed manager Pods have mixed runtime image identities")
		}
	}
	return identity, nil
}

func (r *Runner) waitDatabase(
	ctx context.Context, cfg Config, created *databasev1alpha1.CubridCluster, guard func(context.Context) error,
) (imageIdentity, error) {
	var identity imageIdentity
	err := pollUntil(ctx, cfg.Poll, func() (bool, error) {
		if err := guard(ctx); err != nil {
			return false, err
		}
		cluster := &databasev1alpha1.CubridCluster{}
		if err := r.Client.Get(ctx, client.ObjectKeyFromObject(created), cluster); err != nil {
			return false, err
		}
		if cluster.UID != created.UID {
			return false, errors.New("smoke cluster was replaced")
		}
		if !meta.IsStatusConditionTrue(cluster.Status.Conditions, "Ready") ||
			cluster.Status.ObservedGeneration < cluster.Generation {
			return false, nil
		}
		sts := &appsv1.StatefulSet{}
		if err := r.Client.Get(ctx, client.ObjectKeyFromObject(created), sts); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		if !controllerOwner(sts, "CubridCluster", created.UID) {
			return false, errors.New("database StatefulSet does not belong to the smoke cluster")
		}
		pod := &corev1.Pod{}
		if err := r.Client.Get(ctx, client.ObjectKey{Namespace: cfg.Namespace, Name: clusterName + "-0"}, pod); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		if !controllerOwner(pod, "StatefulSet", sts.UID) {
			return false, errors.New("database Pod does not belong to the smoke StatefulSet")
		}
		if !podReady(pod) {
			return false, nil
		}
		observed, err := podIdentity(pod, databaseContainer, cfg.InstanceManagerImage)
		identity = observed
		return err == nil, err
	})
	if err != nil {
		return identity, fmt.Errorf("wait for the owned database Pod: %w", err)
	}
	return identity, nil
}
