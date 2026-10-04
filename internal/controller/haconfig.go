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
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

const (
	// haGroupName is the group id in ha_node_list (<group>@<host>:...). It is
	// the value the POC clusters formed with (docs/poc/RESULTS.md); each
	// cluster's members are told apart by their host names, not by the group.
	haGroupName = "cubrid"

	// haPortID is the heartbeat UDP port, the CUBRID default the image uses.
	haPortID = 59901

	// haConfFileName is the key of the rendered file in the HA ConfigMap.
	haConfFileName = "cubrid_ha.conf"
)

// haConfigMapName is the ConfigMap holding the cluster's cubrid_ha.conf.
func haConfigMapName(cluster string) string { return cluster + "-ha-config" }

// haMemberNames returns the HA host names of the promotable members: the
// short StatefulSet pod names in ordinal order (ADR-0004). The order is the
// failover priority CUBRID reads from ha_node_list; it does not make ordinal
// 0 the master, which CUBRID decides at runtime (ADR-0001).
func haMemberNames(cluster *databasev1alpha1.CubridCluster) ([]string, error) {
	names := memberNames(cluster, cluster.Spec.Topology.PromotableMembers)
	if len(names) == 0 {
		return nil, fmt.Errorf("topology.promotableMembers must be at least 1 to render ha_node_list")
	}
	for _, name := range names {
		// The same string is the pod name, the OS hostname and the per-member
		// alias Service name, so it has to be a DNS label.
		if errs := validation.IsDNS1123Label(name); len(errs) > 0 {
			return nil, fmt.Errorf("HA member name %q is not a DNS label: %s", name, strings.Join(errs, "; "))
		}
	}
	return names, nil
}

// generateHAConf renders cubrid_ha.conf for an HA cluster. The result depends
// only on the cluster name and spec, so every member gets the same file and it
// does not change when a Pod is replaced or the master moves (ADR-0001,
// ADR-0004). ha_db_list keeps the order of spec.databases (ADR-0010).
func generateHAConf(cluster *databasev1alpha1.CubridCluster) (string, error) {
	if !cluster.Spec.HighAvailability.Enabled {
		return "", fmt.Errorf("highAvailability.enabled is false; a standalone member has no cubrid_ha.conf")
	}
	if len(cluster.Spec.Databases) == 0 {
		return "", fmt.Errorf("databases must name at least one database to render ha_db_list")
	}
	members, err := haMemberNames(cluster)
	if err != nil {
		return "", err
	}
	databases := make([]string, 0, len(cluster.Spec.Databases))
	for _, db := range cluster.Spec.Databases {
		databases = append(databases, db.Name)
	}

	var b strings.Builder
	b.WriteString("[common]\n")
	fmt.Fprintf(&b, "ha_node_list=%s@%s\n", haGroupName, strings.Join(members, ":"))
	fmt.Fprintf(&b, "ha_db_list=%s\n", strings.Join(databases, ","))
	fmt.Fprintf(&b, "ha_port_id=%d\n", haPortID)
	return b.String(), nil
}

// reconcileHAConfig keeps one ConfigMap with the cluster's cubrid_ha.conf, so
// every member reads the same member list. A standalone cluster has none.
func (r *CubridClusterReconciler) reconcileHAConfig(ctx context.Context, cluster *databasev1alpha1.CubridCluster) error {
	if !cluster.Spec.HighAvailability.Enabled {
		return nil
	}
	conf, err := generateHAConf(cluster)
	if err != nil {
		return err
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: haConfigMapName(cluster.Name), Namespace: cluster.Namespace},
	}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Labels = labelsFor(cluster)
		cm.Data = map[string]string{haConfFileName: conf}
		return controllerutil.SetControllerReference(cluster, cm, r.Scheme)
	})
	return err
}
