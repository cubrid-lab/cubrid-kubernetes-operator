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

package lab

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeCluster answers the kubectl calls of this package for one context, the
// one cluster it stands for: an optional marker and at most one Lease.
type fakeCluster struct {
	context string
	marker  string // "" means no marker
	lease   map[string]any
	calls   [][]string
}

func (f *fakeCluster) kubectl(_ context.Context, stdin string, args ...string) (string, error) {
	f.calls = append(f.calls, args)
	if len(args) < 2 || args[0] != "--context" {
		return "", errors.New("call without --context")
	}
	if args[1] != f.context {
		return "", errors.New(`error: context "` + args[1] + `" does not exist`)
	}
	rest := strings.Join(args[2:], " ")
	switch {
	case strings.Contains(rest, "get configmap "+MarkerName):
		if f.marker == "" {
			return "", errors.New(`Error from server (NotFound): configmaps "lab" not found`)
		}
		return f.marker, nil
	case rest == "create -f -":
		if f.lease != nil {
			return "", errors.New(`Error from server (AlreadyExists): leases.coordination.k8s.io "campaign" already exists`)
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(stdin), &obj); err != nil {
			return "", err
		}
		f.lease = obj["spec"].(map[string]any)
		return "lease created", nil
	case strings.Contains(rest, "get lease "+LeaseName) && strings.HasSuffix(rest, "-o json"):
		if f.lease == nil {
			return "", errors.New("NotFound")
		}
		out, _ := json.Marshal(map[string]any{"spec": f.lease})
		return string(out), nil
	case strings.Contains(rest, "get lease "+LeaseName):
		if f.lease == nil {
			return "", nil
		}
		return f.lease["holderIdentity"].(string), nil
	case strings.Contains(rest, "delete lease "+LeaseName):
		f.lease = nil
		return "deleted", nil
	}
	return "", errors.New("unexpected call: " + rest)
}

const (
	labA   = "lab-a"
	labCtx = "lab-ctx"
)

var t0 = time.Date(2026, 10, 15, 6, 0, 0, 0, time.UTC)

func TestCheckAcceptsOnlyTheNamedLab(t *testing.T) {
	ctx := context.Background()
	f := &fakeCluster{context: labCtx, marker: labA}
	if err := (Target{Context: labCtx, Name: labA}).Check(ctx, f.kubectl); err != nil {
		t.Fatalf("the named lab was refused: %v", err)
	}
	for _, tc := range []struct {
		name   string
		target Target
		marker string
		want   string
	}{
		{"another lab", Target{labCtx, "lab-b"}, labA, `leads to the lab "lab-a", not "lab-b"`},
		{"no marker", Target{labCtx, labA}, "", "not treated as a lab"},
		{"unknown context", Target{"kind-x", labA}, labA, `context "kind-x"`},
		{"no context", Target{"", labA}, labA, "LAB_CONTEXT"},
		{"no name", Target{labCtx, ""}, labA, "LAB_TARGET"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeCluster{context: labCtx, marker: tc.marker}
			err := tc.target.Check(ctx, f.kubectl)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Check() = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

// Every call names the context: the caller's current context never decides
// which cluster is used.
func TestEveryCallNamesTheContext(t *testing.T) {
	ctx := context.Background()
	f := &fakeCluster{context: labCtx, marker: labA}
	target := Target{Context: labCtx, Name: labA}
	if err := target.Check(ctx, f.kubectl); err != nil {
		t.Fatal(err)
	}
	lock, err := target.Acquire(ctx, f.kubectl, "a", time.Hour, t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Release(ctx, f.kubectl); err != nil {
		t.Fatal(err)
	}
	for _, call := range f.calls {
		if !slices.Equal(call[:2], []string{"--context", labCtx}) {
			t.Fatalf("call %q does not start with --context lab-ctx", call)
		}
	}
}

func TestASecondCampaignIsRefused(t *testing.T) {
	ctx := context.Background()
	f := &fakeCluster{context: labCtx, marker: labA}
	target := Target{Context: labCtx, Name: labA}

	first, err := target.Acquire(ctx, f.kubectl, "alice@ws/1", time.Hour, t0)
	if err != nil {
		t.Fatalf("the first campaign was refused: %v", err)
	}
	_, err = target.Acquire(ctx, f.kubectl, "bob@ws/2", time.Hour, t0.Add(time.Minute))
	if err == nil {
		t.Fatal("a second campaign was let in while the first held the lab")
	}
	if !strings.Contains(err.Error(), `held by "alice@ws/1"`) || strings.Contains(err.Error(), "delete lease") {
		t.Fatalf("the refusal should name the holder and not suggest removing a live Lease: %v", err)
	}

	if err := first.Release(ctx, f.kubectl); err != nil {
		t.Fatalf("releasing the first campaign: %v", err)
	}
	if _, err := target.Acquire(ctx, f.kubectl, "bob@ws/2", time.Hour, t0.Add(2*time.Minute)); err != nil {
		t.Fatalf("the lab was not free after the release: %v", err)
	}
}

// A Lease past its duration is reported, with the way to remove it, but not
// taken over.
func TestAnExpiredLeaseIsNotTakenOver(t *testing.T) {
	ctx := context.Background()
	f := &fakeCluster{context: labCtx, marker: labA}
	target := Target{Context: labCtx, Name: labA}
	if _, err := target.Acquire(ctx, f.kubectl, "killed-run", time.Hour, t0); err != nil {
		t.Fatal(err)
	}
	_, err := target.Acquire(ctx, f.kubectl, "next-run", time.Hour, t0.Add(2*time.Hour))
	if err == nil {
		t.Fatal("an expired Lease was taken over")
	}
	if !strings.Contains(err.Error(), "may have been killed") ||
		!strings.Contains(err.Error(), "kubectl --context lab-ctx -n cubrid-lab delete lease campaign") {
		t.Fatalf("the refusal should explain the expired Lease and how to remove it: %v", err)
	}
	if got := f.lease["holderIdentity"]; got != "killed-run" {
		t.Fatalf("the Lease holder changed to %v", got)
	}
}

func TestReleaseLeavesAnotherRunsLease(t *testing.T) {
	ctx := context.Background()
	f := &fakeCluster{context: labCtx, marker: labA}
	target := Target{Context: labCtx, Name: labA}
	lock, err := target.Acquire(ctx, f.kubectl, "first", time.Hour, t0)
	if err != nil {
		t.Fatal(err)
	}
	// Someone removed the Lease by hand and another run took the lab.
	f.lease = nil
	if _, err := target.Acquire(ctx, f.kubectl, "second", time.Hour, t0); err != nil {
		t.Fatal(err)
	}
	if err := lock.Release(ctx, f.kubectl); err == nil || !strings.Contains(err.Error(), `held by "second"`) {
		t.Fatalf("Release() = %v, want a refusal naming the new holder", err)
	}
	if f.lease == nil {
		t.Fatal("the other run's Lease was deleted")
	}

	f.lease = nil
	if err := lock.Release(ctx, f.kubectl); err == nil || !strings.Contains(err.Error(), "removed while the run held it") {
		t.Fatalf("Release() of a vanished Lease = %v", err)
	}
}

func TestAcquireRejectsADurationUnderASecond(t *testing.T) {
	f := &fakeCluster{context: labCtx, marker: labA}
	_, err := (Target{labCtx, labA}).Acquire(context.Background(), f.kubectl, "a", 500*time.Millisecond, t0)
	if err == nil {
		t.Fatal("a Lease of zero seconds was created")
	}
	if f.lease != nil {
		t.Fatal("a Lease exists after the refusal")
	}
}
