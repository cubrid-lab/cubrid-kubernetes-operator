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
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults/netfault"
)

// The tests in this file cover -partition-keepalive (#982): before the cut,
// the network partition lane shortens TCP keepalive in every member's
// network namespace and makes the cut-off member's connection to the master
// again, so that a connection left half-open by the cut can end within
// about a minute instead of the kernel's two hours.

func keepaliveConfig(t *testing.T) HAConfig {
	t.Helper()
	cfg := testHAConfig(t)
	cfg.NamespaceMode, cfg.Level, cfg.StablePeriod = FreshNamespace, evidence.LevelVMLab, time.Second
	cfg.Fault, cfg.PartitionHold = FaultPartitionHigherRankIsolated, 2*time.Minute
	cfg.PartitionSettle, cfg.PartitionKeepalive, cfg.Timeout = 10*time.Minute, 30*time.Second, 60*time.Minute
	return cfg
}

// The documented lab command is accepted, and the option's own steps count
// in the -timeout check.
func TestValidateS07PartitionKeepalive(t *testing.T) {
	cfg := keepaliveConfig(t)
	if err := cfg.validate(); err != nil {
		t.Fatalf("the documented lab options are refused: %v", err)
	}
	off := keepaliveConfig(t)
	off.PartitionKeepalive = 0
	if got := cfg.s07StepBounds() - off.s07StepBounds(); got != s07KeepaliveBound {
		t.Errorf("-partition-keepalive adds %s to the steps' bounds, want %s", got, s07KeepaliveBound)
	}
	documented := keepaliveConfig(t)
	documented.DataCheckWithin = 2 * time.Minute
	if got := documented.s07StepBounds(); got != 41*time.Minute {
		t.Errorf("the documented command's own bounds are %s, the guide says 41m", got)
	}
	cfg.Timeout = cfg.s07StepBounds()
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "-timeout") {
		t.Errorf("a timeout that only covers the steps' bounds: %v", err)
	}
	refused := map[string]func(*HAConfig){
		"no partition fault":   func(c *HAConfig) { c.Fault = "" },
		"master-isolated":      func(c *HAConfig) { c.Fault = FaultPartitionMasterIsolated },
		"a negative time":      func(c *HAConfig) { c.PartitionKeepalive = -time.Second },
		"part of a second":     func(c *HAConfig) { c.PartitionKeepalive = 1500 * time.Millisecond },
		"the kernel's default": func(c *HAConfig) { c.PartitionKeepalive = 2 * time.Hour },
	}
	for name, change := range refused {
		t.Run(name, func(t *testing.T) {
			c := keepaliveConfig(t)
			change(&c)
			if err := c.validate(); err == nil || !strings.Contains(err.Error(), "-partition-keepalive") {
				t.Errorf("accepted, or refused for another reason: %v", err)
			}
		})
	}
	slave := keepaliveConfig(t)
	slave.Fault = FaultPartitionSlaveIsolated
	if err := slave.validate(); err != nil {
		t.Errorf("slave-isolated with -partition-keepalive: %v", err)
	}
}

// The values go to every member the partition enters, the cut-off member's
// connection that is made again is the one to the master, and whatever
// happened is kept in the record. A setting that cannot be applied is an
// error, which blocks S07 before anything is cut.
func TestShortenS07KeepaliveRecordsAndRefuses(t *testing.T) {
	cfg := keepaliveConfig(t)
	var ran []string
	command := func(_ context.Context, args ...string) (string, error) {
		ran = append(ran, strings.Join(args, " "))
		return "", errors.New("unreachable")
	}
	h := &haRun{Runner: &Runner{Client: testClient(t, cfg.Config, interceptor.Funcs{}), Command: command}, cfg: cfg,
		members: testS07Members}
	endpoint := func(name string) netfault.Endpoint {
		return netfault.Endpoint{Name: name, Namespace: cfg.Namespace, Pod: name, UID: name,
			IP: "10.42.0." + name[len(name)-1:], Port: s07MemberPort}
	}
	p := &netfault.Partition{Env: netfault.Env{Client: h.Client, Kubectl: command, Poll: time.Millisecond},
		Target: endpoint(m0), Cuts: []netfault.Cut{{Peer: endpoint(m1), Direction: netfault.Both},
			{Peer: endpoint(m2), Direction: netfault.Both},
			{Peer: netfault.Endpoint{Name: testManagerName, IP: "10.42.0.8", Port: s07OperatorPort, NoEnter: true}}}}
	rec := &s07Record{Variant: s07HigherRankIsolated, Master: m1}
	err := h.shortenS07Keepalive(context.Background(), rec, p)
	if err == nil {
		t.Fatal("a keepalive that could not be set was accepted")
	}
	k := rec.Keepalive
	if k == nil || k.Unmet == "" || k.Target != m0 || k.Master != m1 ||
		k.Requested != (netfault.KeepaliveValues{Time: 30, Interval: 10, Probes: 3}) {
		t.Fatalf("record %+v", k)
	}
	if len(k.Members) != 1 || k.Members[0].Name != m0 || k.Members[0].Error == "" {
		t.Errorf("members %+v: the first member's failure was not recorded", k.Members)
	}
	const unnamed = "ha-5"
	rec = &s07Record{Variant: s07HigherRankIsolated, Master: unnamed}
	if err := h.shortenS07Keepalive(context.Background(), rec, p); err == nil ||
		!strings.Contains(err.Error(), unnamed) || rec.Keepalive == nil || rec.Keepalive.Unmet == "" {
		t.Errorf("a master the partition does not name: %v, record %+v", err, rec.Keepalive)
	}
}
