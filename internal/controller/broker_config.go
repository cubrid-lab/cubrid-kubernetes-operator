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
	"fmt"
	"strings"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

const (
	// brokerRWPort / brokerROPort are the CAS ports for the RW and RO brokers.
	brokerRWPort = 33000
	brokerROPort = 33001
	// brokerAccessModeLabel selects a broker pod's access mode on the Services.
	brokerAccessModeLabel = "database.cubrid.io/broker-access-mode"
	componentBroker       = "broker"
)

// brokerLabelsFor returns the selector labels for a cluster's broker tier
// (component=broker), distinct from the database instances (ADR-0002).
func brokerLabelsFor(cluster *databasev1alpha1.CubridCluster) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       appName,
		"app.kubernetes.io/instance":   cluster.Name,
		"app.kubernetes.io/component":  componentBroker,
		"app.kubernetes.io/managed-by": "cubrid-kubernetes-operator",
	}
}

// memberDNSNames returns the stable per-member DNS names of all promotable DB
// instances via the headless <cluster>-instances Service (ADR-0004). The broker
// databases.txt db-host field lists these so RW brokers seek the master
// natively and follow failover through the host list (ADR-0002).
func memberDNSNames(cluster *databasev1alpha1.CubridCluster) []string {
	svc := instancesServiceName(cluster.Name)
	names := memberNames(cluster, cluster.Spec.Topology.PromotableMembers)
	out := make([]string, 0, len(names))
	for _, m := range names {
		out = append(out, fmt.Sprintf("%s.%s.%s.svc", m, svc, cluster.Namespace))
	}
	return out
}

// generateBrokerConf renders cubrid_broker.conf with an RW broker
// (ACCESS_MODE=RW) and an RO broker (ACCESS_MODE=RO), matching the topology
// proven in POC-8 (ADR-0002).
func generateBrokerConf() string {
	var b strings.Builder
	b.WriteString("[broker]\n")
	b.WriteString("MASTER_SHM_ID           =30001\n")
	b.WriteString("ADMIN_LOG_FILE          =log/broker/cubrid_broker.log\n\n")
	writeBrokerSection(&b, "RW", brokerRWPort, "RW")
	writeBrokerSection(&b, "RO", brokerROPort, "RO")
	return b.String()
}

func writeBrokerSection(b *strings.Builder, name string, port int, accessMode string) {
	fmt.Fprintf(b, "[%%%s]\n", name)
	b.WriteString("SERVICE                 =ON\n")
	fmt.Fprintf(b, "BROKER_PORT             =%d\n", port)
	b.WriteString("MIN_NUM_APPL_SERVER     =2\n")
	b.WriteString("MAX_NUM_APPL_SERVER     =5\n")
	fmt.Fprintf(b, "APPL_SERVER_SHM_ID      =%d\n", port)
	b.WriteString("LOG_DIR                 =log/broker/sql_log\n")
	b.WriteString("ERROR_LOG_DIR           =log/broker/error_log\n")
	b.WriteString("SQL_LOG                 =ON\n")
	fmt.Fprintf(b, "ACCESS_MODE             =%s\n\n", accessMode)
}

// generateBrokerDatabasesTxt renders the broker databases.txt whose db-host
// field lists every promotable DB member (ADR-0002/0004). This is broker→DB
// routing metadata; the RW broker seeks the master across this host list.
func generateBrokerDatabasesTxt(cluster *databasev1alpha1.CubridCluster) string {
	db := ""
	if len(cluster.Spec.Databases) > 0 {
		db = cluster.Spec.Databases[0].Name
	}
	hosts := strings.Join(memberDNSNames(cluster), ":")
	var b strings.Builder
	b.WriteString("#db-name\tvol-path\tdb-host\tlog-path\tlob-base-path\n")
	fmt.Fprintf(&b, "%s\t/home/cubrid/CUBRID/databases/%s\t%s\t/home/cubrid/CUBRID/databases/%s\tfile:/home/cubrid/CUBRID/databases/%s/lob\n",
		db, db, hosts, db, db)
	return b.String()
}
