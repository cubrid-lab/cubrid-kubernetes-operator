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

// Package lab guards runs against the shared multi-VM lab (#80, #122). A run
// names the kubectl context it uses and the lab it expects to find there; it
// starts only when the cluster behind that context carries the marker of that
// lab, and only while no other run holds the lab's campaign Lease.
//
// Everything goes through kubectl with an explicit --context, and with
// --kubeconfig when the run has pinned its own copy of that context, so a run
// never depends on, or changes, the caller's current context.
package lab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	// Namespace holds the marker and the campaign Lease.
	Namespace = "cubrid-lab"
	// MarkerName is the ConfigMap whose data key MarkerKey names the lab.
	MarkerName = "lab"
	MarkerKey  = "name"
	// LeaseName is the Lease a campaign holds while it runs.
	LeaseName = "campaign"
)

// Holder is the holderIdentity this entry point writes in the Lease: who
// started the run, on which host, its process, and when.
func Holder(user, host string, pid int, at time.Time) string {
	return fmt.Sprintf("%s@%s/%d/%s", user, host, pid, at.UTC().Format(time.RFC3339))
}

// ownHolder matches the holders Holder writes. Other tools, such as the
// canonical campaign launcher, take the same Lease with their own holders.
var ownHolder = regexp.MustCompile(`^[^@/\s]+@[^/\s]*/[0-9]+/[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$`)

// Kubectl runs kubectl with args, passing stdin when it is not empty, and
// returns its standard output. An error carries what kubectl wrote to its
// standard error.
type Kubectl func(ctx context.Context, stdin string, args ...string) (string, error)

// Target is the lab a run is meant for.
type Target struct {
	// Context is the kubectl context the run uses.
	Context string
	// Name is the lab that context must lead to, as its marker names it.
	Name string
	// Kubeconfig, when set, is the file every call reads the context from,
	// instead of the caller's KUBECONFIG: a copy taken once at the start, so
	// that redefining the context during a run cannot move the run, or the
	// release of its Lease, to another cluster.
	Kubeconfig string
}

func (t Target) kubectl(ctx context.Context, k Kubectl, stdin string, args ...string) (string, error) {
	prefix := []string{"--context", t.Context}
	if t.Kubeconfig != "" {
		prefix = append([]string{"--kubeconfig", t.Kubeconfig}, prefix...)
	}
	return k(ctx, stdin, append(prefix, args...)...)
}

// Check returns an error unless the cluster behind t.Context carries the
// marker of t.Name. A cluster without a marker is not a lab, whatever its
// context is called.
func (t Target) Check(ctx context.Context, k Kubectl) error {
	if t.Context == "" || t.Name == "" {
		return errors.New("a lab run needs both a kubectl context and the name of the lab (LAB_CONTEXT, LAB_TARGET)")
	}
	out, err := t.kubectl(ctx, k, "", "-n", Namespace, "get", "configmap", MarkerName,
		"-o", "jsonpath={.data."+MarkerKey+"}")
	if err != nil {
		return fmt.Errorf("context %q: reading the lab marker %s/configmap/%s: %w; "+
			"a cluster without it is not treated as a lab", t.Context, Namespace, MarkerName, err)
	}
	if got := strings.TrimSpace(out); got != t.Name {
		return fmt.Errorf("context %q leads to the lab %q, not %q", t.Context, got, t.Name)
	}
	return nil
}

// Lock is a held campaign Lease.
type Lock struct {
	target Target
	holder string
	// uid and version are the Lease's metadata.uid and resourceVersion as
	// this run created it: Release deletes that object only, and only while
	// nobody has written it since.
	uid     string
	version string
}

// Acquire creates the campaign Lease for holder, so that a second campaign on
// the same lab is refused instead of interleaving with this one. Creating an
// object is atomic in the API server: of two runs that try at once, one
// fails with AlreadyExists.
//
// A Lease left behind by a run that was killed is not taken over: whether
// that campaign still runs cannot be told from here, so the error names its
// holder and how to remove it once that is known.
//
// When the create fails in another way, for example a timeout after the API
// server stored the Lease, the Lease is read back: if it names holder, it is
// this run's and the lock is returned, so that it is released as usual.
func (t Target) Acquire(
	ctx context.Context, k Kubectl, holder string, duration time.Duration, now time.Time,
) (*Lock, error) {
	seconds := int(duration / time.Second)
	if seconds < 1 {
		return nil, fmt.Errorf("the campaign duration %s is shorter than a second", duration)
	}
	lease := map[string]any{
		"apiVersion": "coordination.k8s.io/v1",
		"kind":       "Lease",
		"metadata":   map[string]any{"name": LeaseName, "namespace": Namespace},
		"spec": map[string]any{
			"holderIdentity":       holder,
			"leaseDurationSeconds": seconds,
			"acquireTime":          now.UTC().Format("2006-01-02T15:04:05.000000Z"),
			"renewTime":            now.UTC().Format("2006-01-02T15:04:05.000000Z"),
		},
	}
	manifest, err := json.Marshal(lease)
	if err != nil {
		return nil, err
	}
	out, err := t.kubectl(ctx, k, string(manifest), "create", "-f", "-",
		"-o", "jsonpath={.metadata.uid} {.metadata.resourceVersion}")
	if err == nil {
		meta := strings.Fields(out)
		if len(meta) != 2 {
			return nil, fmt.Errorf("context %q: the campaign Lease was created without a uid and a resourceVersion: %q",
				t.Context, out)
		}
		return &Lock{target: t, holder: holder, uid: meta[0], version: meta[1]}, nil
	}
	if strings.Contains(err.Error(), "AlreadyExists") {
		return nil, t.held(ctx, k, holder, now)
	}
	// The create may still have been stored.
	l, readErr := t.lease(ctx, k)
	switch {
	case readErr == nil && l.Spec.Holder == holder && l.Metadata.UID != "" && l.Metadata.ResourceVersion != "":
		return &Lock{target: t, holder: holder, uid: l.Metadata.UID, version: l.Metadata.ResourceVersion}, nil
	case readErr == nil && l.Spec.Holder != "":
		return nil, fmt.Errorf("context %q: creating the campaign Lease: %w; %w", t.Context, err, t.describe(l, now))
	default:
		return nil, fmt.Errorf("context %q: creating the campaign Lease: %w; "+
			"whether the Lease exists is unknown, check with: kubectl --context %s -n %s get lease %s",
			t.Context, err, t.Context, Namespace, LeaseName)
	}
}

type leaseObject struct {
	Metadata struct {
		UID             string `json:"uid"`
		ResourceVersion string `json:"resourceVersion"`
	} `json:"metadata"`
	Spec struct {
		Holder   string    `json:"holderIdentity"`
		Seconds  int       `json:"leaseDurationSeconds"`
		Acquired time.Time `json:"acquireTime"`
		Renewed  time.Time `json:"renewTime"`
	} `json:"spec"`
}

// lease reads the campaign Lease.
func (t Target) lease(ctx context.Context, k Kubectl) (leaseObject, error) {
	var l leaseObject
	out, err := t.kubectl(ctx, k, "", "-n", Namespace, "get", "lease", LeaseName, "-o", "json")
	if err != nil {
		return l, err
	}
	if err := json.Unmarshal([]byte(out), &l); err != nil {
		return l, fmt.Errorf("parsing the Lease: %w", err)
	}
	return l, nil
}

// held describes the Lease another campaign holds.
func (t Target) held(ctx context.Context, k Kubectl, holder string, now time.Time) error {
	l, err := t.lease(ctx, k)
	if err != nil {
		return fmt.Errorf("the lab %q is held by another campaign (its Lease could not be read: %w)", t.Name, err)
	}
	if l.Spec.Holder == holder {
		// Two Acquire calls of one run; a run calls it once.
		return fmt.Errorf("the lab %q is already held by this run", t.Name)
	}
	return t.describe(l, now)
}

// describe says who holds the lab and, for a Lease of this entry point past
// its end, how to remove it. A Lease ends its duration after its last renewal
// (its acquisition when it has none). Another tool's Lease is never advised
// for removal: that tool may renew it, or have its own recovery, and only it
// can tell whether its campaign still runs.
func (t Target) describe(l leaseObject, now time.Time) error {
	msg := fmt.Sprintf("the lab %q is held by %q since %s", t.Name, l.Spec.Holder,
		l.Spec.Acquired.UTC().Format(time.RFC3339))
	if !ownHolder.MatchString(l.Spec.Holder) {
		return errors.New(msg + "; that Lease was not taken by this entry point, so ask its holder, " +
			"or follow the recovery of the tool that took it, before touching it")
	}
	last := l.Spec.Renewed
	if last.IsZero() {
		last = l.Spec.Acquired
	}
	if end := last.Add(time.Duration(l.Spec.Seconds) * time.Second); now.After(end) {
		msg += fmt.Sprintf("; it was due to end at %s, so that run may have been killed. "+
			"Once it is certain no campaign runs, remove the Lease: kubectl --context %s -n %s delete lease %s",
			end.UTC().Format(time.RFC3339), t.Context, Namespace, LeaseName)
	}
	return errors.New(msg)
}

// Release deletes the Lease, but only the object Acquire created, as it
// created it: the delete carries its uid and resourceVersion as
// preconditions, so a Lease that someone removed and another run created
// since, or that another holder wrote in place, is left alone, and nothing is
// read before the delete that could change under it.
func (l *Lock) Release(ctx context.Context, k Kubectl) error {
	options, err := json.Marshal(map[string]any{
		"apiVersion":    "v1",
		"kind":          "DeleteOptions",
		"preconditions": map[string]any{"uid": l.uid, "resourceVersion": l.version},
	})
	if err != nil {
		return err
	}
	path := "/apis/coordination.k8s.io/v1/namespaces/" + Namespace + "/leases/" + LeaseName
	_, err = l.target.kubectl(ctx, k, string(options), "delete", "--raw", path, "-f", "-")
	switch {
	case err == nil:
		return nil
	case strings.Contains(err.Error(), "(NotFound)"):
		return fmt.Errorf("the campaign Lease of %q was removed while the run held it", l.holder)
	case strings.Contains(err.Error(), "(Conflict)"):
		return fmt.Errorf("the campaign Lease is no longer the one %q created (uid %s, resourceVersion %s); left in place",
			l.holder, l.uid, l.version)
	default:
		return fmt.Errorf("deleting the campaign Lease: %w", err)
	}
}
