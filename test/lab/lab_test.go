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
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeCluster answers the kubectl calls of this package for one context, the
// one cluster it stands for: an optional marker and at most one Lease.
type fakeCluster struct {
	context    string
	kubeconfig string // when set, every call must name this file
	marker     string // "" means no marker
	lease      map[string]any
	uid        string
	uids       int
	// createErr, when set, fails the next create after storing the Lease, as
	// a timeout after the API server stored it would.
	createErr error
	calls     [][]string
}

func (f *fakeCluster) kubectl(_ context.Context, stdin string, args ...string) (string, error) {
	f.calls = append(f.calls, args)
	if f.kubeconfig != "" {
		if len(args) < 2 || args[0] != "--kubeconfig" || args[1] != f.kubeconfig {
			return "", errors.New("call without --kubeconfig " + f.kubeconfig)
		}
		args = args[2:]
	}
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
	case rest == "create -f - -o jsonpath={.metadata.uid}":
		if f.lease != nil {
			return "", errors.New(`Error from server (AlreadyExists): leases.coordination.k8s.io "campaign" already exists`)
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(stdin), &obj); err != nil {
			return "", err
		}
		f.store(obj["spec"].(map[string]any))
		if err := f.createErr; err != nil {
			f.createErr = nil
			return "", err
		}
		return f.uid, nil
	case rest == "-n "+Namespace+" get lease "+LeaseName+" -o json":
		if f.lease == nil {
			return "", errors.New(`Error from server (NotFound): leases.coordination.k8s.io "campaign" not found`)
		}
		out, _ := json.Marshal(map[string]any{"metadata": map[string]any{"uid": f.uid}, "spec": f.lease})
		return string(out), nil
	case rest == "delete --raw /apis/coordination.k8s.io/v1/namespaces/"+Namespace+"/leases/"+LeaseName+" -f -":
		var options struct {
			Preconditions struct {
				UID string `json:"uid"`
			} `json:"preconditions"`
		}
		if err := json.Unmarshal([]byte(stdin), &options); err != nil {
			return "", err
		}
		switch {
		case f.lease == nil:
			return "", errors.New(`Error from server (NotFound): leases.coordination.k8s.io "campaign" not found`)
		case options.Preconditions.UID != f.uid:
			return "", fmt.Errorf(`Error from server (Conflict): Operation cannot be fulfilled on `+
				`leases.coordination.k8s.io "campaign": Precondition failed: `+
				`UID in precondition: %s, UID in object meta: %s`, options.Preconditions.UID, f.uid)
		}
		f.lease = nil
		return "", nil
	}
	return "", errors.New("unexpected call: " + rest)
}

// store creates the Lease with a new uid.
func (f *fakeCluster) store(spec map[string]any) {
	f.uids++
	f.lease = spec
	f.uid = fmt.Sprintf("uid-%d", f.uids)
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
		{"another lab", Target{Context: labCtx, Name: "lab-b"}, labA, `leads to the lab "lab-a", not "lab-b"`},
		{"no marker", Target{Context: labCtx, Name: labA}, "", "not treated as a lab"},
		{"unknown context", Target{Context: "kind-x", Name: labA}, labA, `context "kind-x"`},
		{"no context", Target{Context: "", Name: labA}, labA, "LAB_CONTEXT"},
		{"no name", Target{Context: labCtx, Name: ""}, labA, "LAB_TARGET"},
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

// Every call names the context, and the pinned kubeconfig when there is one:
// neither the caller's current context nor a redefinition of the context in
// the caller's KUBECONFIG decides which cluster is used.
func TestEveryCallNamesTheContext(t *testing.T) {
	for _, kubeconfig := range []string{"", "/tmp/pinned/kubeconfig"} {
		t.Run("kubeconfig="+kubeconfig, func(t *testing.T) {
			ctx := context.Background()
			f := &fakeCluster{context: labCtx, kubeconfig: kubeconfig, marker: labA}
			target := Target{Context: labCtx, Name: labA, Kubeconfig: kubeconfig}
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
			want := []string{"--context", labCtx}
			if kubeconfig != "" {
				want = append([]string{"--kubeconfig", kubeconfig}, want...)
			}
			for _, call := range f.calls {
				if !slices.Equal(call[:len(want)], want) {
					t.Fatalf("call %q does not start with %q", call, want)
				}
			}
		})
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
	if err := lock.Release(ctx, f.kubectl); err == nil || !strings.Contains(err.Error(), "left in place") {
		t.Fatalf("Release() = %v, want a refusal that leaves the other Lease", err)
	}
	if f.lease == nil || f.lease["holderIdentity"] != "second" {
		t.Fatal("the other run's Lease was deleted")
	}

	f.lease = nil
	if err := lock.Release(ctx, f.kubectl); err == nil || !strings.Contains(err.Error(), "removed while the run held it") {
		t.Fatalf("Release() of a vanished Lease = %v", err)
	}
}

// Release deletes by uid alone: it reads nothing first, so nothing can
// change between a check and the delete.
func TestReleaseDeletesOnlyByUID(t *testing.T) {
	ctx := context.Background()
	f := &fakeCluster{context: labCtx, marker: labA}
	lock, err := (Target{Context: labCtx, Name: labA}).Acquire(ctx, f.kubectl, "a", time.Hour, t0)
	if err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	if err := lock.Release(ctx, f.kubectl); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || !slices.Contains(f.calls[0], "--raw") {
		t.Fatalf("Release() made the calls %q, want one delete with a uid precondition", f.calls)
	}
}

// A create that fails after the API server stored the Lease leaves this
// run's Lease behind; Acquire finds it and returns the lock, so that the run
// releases it.
func TestAnAmbiguousCreateKeepsThisRunsLease(t *testing.T) {
	ctx := context.Background()
	f := &fakeCluster{context: labCtx, marker: labA,
		createErr: errors.New("Unable to connect to the server: net/http: request canceled (Client.Timeout exceeded)")}
	target := Target{Context: labCtx, Name: labA}
	lock, err := target.Acquire(ctx, f.kubectl, "mine", time.Hour, t0)
	if err != nil {
		t.Fatalf("Acquire() = %v, want the lock of the stored Lease", err)
	}
	if err := lock.Release(ctx, f.kubectl); err != nil {
		t.Fatal(err)
	}
	if f.lease != nil {
		t.Fatal("the Lease is still there after the release")
	}

	// When the stored Lease is another run's, the error names its holder.
	other := &fakeCluster{context: labCtx, marker: labA}
	theirs := func(ctx context.Context, stdin string, args ...string) (string, error) {
		if slices.Contains(args, "create") {
			other.store(map[string]any{"holderIdentity": "other", "leaseDurationSeconds": 3600, "acquireTime": t0})
			return "", errors.New("timeout")
		}
		return other.kubectl(ctx, stdin, args...)
	}
	if _, err := target.Acquire(ctx, theirs, "mine", time.Hour, t0); err == nil ||
		!strings.Contains(err.Error(), `held by "other"`) {
		t.Fatalf("Acquire() = %v, want an error naming the holder", err)
	}

	// When there is no Lease to read, the error says its state is unknown.
	none := &fakeCluster{context: labCtx, marker: labA}
	lost := func(ctx context.Context, stdin string, args ...string) (string, error) {
		if slices.Contains(args, "create") {
			return "", errors.New("timeout")
		}
		return none.kubectl(ctx, stdin, args...)
	}
	if _, err := target.Acquire(ctx, lost, "mine", time.Hour, t0); err == nil ||
		!strings.Contains(err.Error(), "whether the Lease exists is unknown") {
		t.Fatalf("Acquire() = %v, want an error saying the Lease state is unknown", err)
	}
}

func TestAcquireRejectsADurationUnderASecond(t *testing.T) {
	f := &fakeCluster{context: labCtx, marker: labA}
	_, err := (Target{Context: labCtx, Name: labA}).Acquire(context.Background(), f.kubectl, "a", 500*time.Millisecond, t0)
	if err == nil {
		t.Fatal("a Lease of zero seconds was created")
	}
	if f.lease != nil {
		t.Fatal("a Lease exists after the refusal")
	}
}
