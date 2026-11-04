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
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
)

// applierReading is the last reading of a slave's applier that the stall rule
// compares the next one with (nextReplication). Its positions change every few
// seconds on a healthy cluster, also an idle one (docs/poc/RESULTS.md,
// POC-37 part D and POC-48), so it is kept in memory and not in the status:
// in the status it would make every reconcile write it.
type applierReading struct {
	Source  string
	Applier string
	// FinalLSA is the applier's position in the copied log and CopiedEOF the
	// end of that copy; nil when the Instance Manager did not report them.
	FinalLSA     *databasev1alpha1.LogAddress
	CopiedEOF    *databasev1alpha1.LogAddress
	DelayedPages int64
	ReadAt       time.Time
}

// ApplierPositions keeps, per cluster UID and member, the last reading of each
// slave's applier (#249). The cluster controller writes it, after the status
// that the readings fed was written; the backup controller only reads it to
// judge standbys the same way. It is lost when the Operator restarts or
// another instance becomes leader: the first reading after that has nothing to
// compare with, and the stalledSince stored in the status is kept until the
// next one. The zero value is empty and ready to use; a nil store holds
// nothing.
type ApplierPositions struct {
	mu       sync.Mutex
	clusters map[types.UID]clusterPositions
}

type clusterPositions struct {
	name    types.NamespacedName
	members map[string]applierReading
}

// NewApplierPositions returns an empty store, to be shared by the cluster and
// backup controllers.
func NewApplierPositions() *ApplierPositions { return &ApplierPositions{} }

// readings returns a copy of the readings kept for the cluster uid, by member,
// without those older than replicationHistoryTTL at now or stamped more than
// roleObservationTTL after it.
func (p *ApplierPositions) readings(uid types.UID, now time.Time) map[string]applierReading {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]applierReading)
	for m, r := range p.clusters[uid].members {
		if age := now.Sub(r.ReadAt); age <= replicationHistoryTTL && age >= -roleObservationTTL {
			out[m] = r.deepCopy()
		}
	}
	return out
}

// commit replaces the readings kept for the cluster uid, named name, with
// readings: a member left out of it is forgotten, and so is an earlier
// cluster of the same name.
func (p *ApplierPositions) commit(uid types.UID, name types.NamespacedName, readings map[string]applierReading) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for other, c := range p.clusters {
		if other != uid && c.name == name {
			delete(p.clusters, other)
		}
	}
	if len(readings) == 0 {
		delete(p.clusters, uid)
		return
	}
	if p.clusters == nil {
		p.clusters = make(map[types.UID]clusterPositions)
	}
	members := make(map[string]applierReading, len(readings))
	for m, r := range readings {
		members[m] = r.deepCopy()
	}
	p.clusters[uid] = clusterPositions{name: name, members: members}
}

// forget drops every reading of the clusters named name, as when the cluster
// is not found.
func (p *ApplierPositions) forget(name types.NamespacedName) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for uid, c := range p.clusters {
		if c.name == name {
			delete(p.clusters, uid)
		}
	}
}

func (r applierReading) deepCopy() applierReading {
	r.FinalLSA, r.CopiedEOF = r.FinalLSA.DeepCopy(), r.CopiedEOF.DeepCopy()
	return r
}

// readingPositions returns the readings of this observation's slaves to keep
// for the next one. An authoritative slave whose applier was read keeps this
// reading, read at now, unless the reading kept is less than
// positionCompareInterval old: that one stays the base the next reading is
// compared with (nextReplication). A member authoritatively observed in
// another role keeps none. Any other member, a slave whose applier was not
// read or a member that gave no usable answer, keeps the reading kept before,
// until it is older than replicationHistoryTTL.
func readingPositions(members []string, obs map[string]RoleObservation, before map[string]applierReading,
	now time.Time) map[string]applierReading {
	out := make(map[string]applierReading)
	for _, m := range members {
		o, observed := obs[m]
		kept, hasKept := before[m]
		switch {
		case observed && authoritative(o, now) && o.Role != databasev1alpha1.RoleSlave:
		case observed && authoritative(o, now) && o.Replication != nil && (!hasKept || !tooClose(kept.ReadAt, now)):
			r := o.Replication
			out[m] = applierReading{Source: r.Source, Applier: r.Applier, FinalLSA: r.FinalLSA.DeepCopy(),
				CopiedEOF: r.CopiedEOF.DeepCopy(), DelayedPages: r.DelayedPages, ReadAt: now}
		case hasKept:
			out[m] = kept
		}
	}
	return out
}

// positionCompareInterval is the least time between two readings whose
// applier positions are compared. Reconciles that events trigger can read a
// slave a second apart, too soon for a busy applier's position to have moved,
// which would start a stall and clear it at the next reading.
const positionCompareInterval = haResyncInterval / 2

// tooClose reports whether a reading taken at now is too close to one read at
// before to compare their positions.
func tooClose(before, now time.Time) bool {
	age := now.Sub(before)
	return age >= 0 && age < positionCompareInterval
}

// positionOf returns the reading kept for member, nil when there is none.
func positionOf(positions map[string]applierReading, member string) *applierReading {
	if r, ok := positions[member]; ok {
		return &r
	}
	return nil
}
