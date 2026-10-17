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
// Everything goes through kubectl with an explicit --context, so a run never
// depends on, or changes, the caller's current context.
package lab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
}

func (t Target) kubectl(ctx context.Context, k Kubectl, stdin string, args ...string) (string, error) {
	return k(ctx, stdin, append([]string{"--context", t.Context}, args...)...)
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
}

// Acquire creates the campaign Lease for holder, so that a second campaign on
// the same lab is refused instead of interleaving with this one. Creating an
// object is atomic in the API server: of two runs that try at once, one
// fails with AlreadyExists.
//
// A Lease left behind by a run that was killed is not taken over: whether
// that campaign still runs cannot be told from here, so the error names its
// holder and how to remove it once that is known.
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
	if _, err := t.kubectl(ctx, k, string(manifest), "create", "-f", "-"); err != nil {
		if !strings.Contains(err.Error(), "AlreadyExists") {
			return nil, fmt.Errorf("context %q: creating the campaign Lease: %w", t.Context, err)
		}
		return nil, t.held(ctx, k, now)
	}
	return &Lock{target: t, holder: holder}, nil
}

// held describes the Lease another campaign holds.
func (t Target) held(ctx context.Context, k Kubectl, now time.Time) error {
	out, err := t.kubectl(ctx, k, "", "-n", Namespace, "get", "lease", LeaseName, "-o", "json")
	if err != nil {
		return fmt.Errorf("the lab %q is held by another campaign (its Lease could not be read: %w)", t.Name, err)
	}
	var l struct {
		Spec struct {
			Holder   string    `json:"holderIdentity"`
			Seconds  int       `json:"leaseDurationSeconds"`
			Acquired time.Time `json:"acquireTime"`
		} `json:"spec"`
	}
	if err := json.Unmarshal([]byte(out), &l); err != nil {
		return fmt.Errorf("the lab %q is held by another campaign (its Lease could not be parsed: %w)", t.Name, err)
	}
	msg := fmt.Sprintf("the lab %q is held by %q since %s", t.Name, l.Spec.Holder,
		l.Spec.Acquired.UTC().Format(time.RFC3339))
	if end := l.Spec.Acquired.Add(time.Duration(l.Spec.Seconds) * time.Second); now.After(end) {
		msg += fmt.Sprintf("; it was due to end at %s, so that run may have been killed. "+
			"Once it is certain no campaign runs, remove the Lease: kubectl --context %s -n %s delete lease %s",
			end.UTC().Format(time.RFC3339), t.Context, Namespace, LeaseName)
	}
	return errors.New(msg)
}

// Release deletes the Lease, but only while it is still this lock's: a Lease
// that someone removed and another run created since is left alone. The
// check and the delete are two calls, so a Lease replaced between them would
// still be deleted; replacing one means deleting it by hand first, which the
// error of Acquire only suggests once it has expired.
func (l *Lock) Release(ctx context.Context, k Kubectl) error {
	out, err := l.target.kubectl(ctx, k, "", "-n", Namespace, "get", "lease", LeaseName,
		"--ignore-not-found", "-o", "jsonpath={.spec.holderIdentity}")
	if err != nil {
		return fmt.Errorf("reading the campaign Lease before releasing it: %w", err)
	}
	switch got := strings.TrimSpace(out); got {
	case "":
		return fmt.Errorf("the campaign Lease of %q was removed while the run held it", l.holder)
	case l.holder:
	default:
		return fmt.Errorf("the campaign Lease is now held by %q, not %q; left in place", got, l.holder)
	}
	if _, err := l.target.kubectl(ctx, k, "", "-n", Namespace, "delete", "lease", LeaseName,
		"--ignore-not-found"); err != nil {
		return fmt.Errorf("deleting the campaign Lease: %w", err)
	}
	return nil
}
