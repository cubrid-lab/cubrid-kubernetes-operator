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
	// version is the Lease's resourceVersion; every write raises it.
	version int
	// createErr, when set, fails the next create after storing the Lease, as
	// a timeout after the API server stored it would.
	createErr error
	// renewErr, when set, fails the next renewal after storing it.
	renewErr error
	calls    [][]string
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
	case rest == "create -f - -o jsonpath={.metadata.uid} {.metadata.resourceVersion}":
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
		return f.uid + " " + f.resourceVersion(), nil
	case rest == "-n "+Namespace+" get lease "+LeaseName+" -o json":
		if f.lease == nil {
			return "", errors.New(`Error from server (NotFound): leases.coordination.k8s.io "campaign" not found`)
		}
		out, _ := json.Marshal(map[string]any{
			"metadata": map[string]any{"uid": f.uid, "resourceVersion": f.resourceVersion()},
			"spec":     f.lease,
		})
		return string(out), nil
	case rest == "replace --raw /apis/coordination.k8s.io/v1/namespaces/"+Namespace+"/leases/"+LeaseName+" -f -":
		var obj struct {
			Metadata struct {
				UID             string `json:"uid"`
				ResourceVersion string `json:"resourceVersion"`
			} `json:"metadata"`
			Spec map[string]any `json:"spec"`
		}
		if err := json.Unmarshal([]byte(stdin), &obj); err != nil {
			return "", err
		}
		// As the API server answers: a uid in the body is a precondition, so a
		// removed Lease is a Conflict, not NotFound.
		switch {
		case f.lease == nil || obj.Metadata.UID != f.uid:
			return "", fmt.Errorf(`Error from server (Conflict): Operation cannot be fulfilled on `+
				`leases.coordination.k8s.io "campaign": Precondition failed: `+
				`UID in precondition: %s, UID in object meta: %s`, obj.Metadata.UID, f.uid)
		case obj.Metadata.ResourceVersion != f.resourceVersion():
			return "", errors.New(`Error from server (Conflict): Operation cannot be fulfilled on ` +
				`leases.coordination.k8s.io "campaign": the object has been modified; ` +
				`please apply your changes to the latest version and try again`)
		}
		f.update(obj.Spec)
		if err := f.renewErr; err != nil {
			f.renewErr = nil
			return "", err
		}
		out, _ := json.Marshal(map[string]any{
			"metadata": map[string]any{"uid": f.uid, "resourceVersion": f.resourceVersion()},
			"spec":     f.lease,
		})
		return string(out), nil
	case rest == "delete --raw /apis/coordination.k8s.io/v1/namespaces/"+Namespace+"/leases/"+LeaseName+" -f -":
		var options struct {
			Preconditions struct {
				UID             string `json:"uid"`
				ResourceVersion string `json:"resourceVersion"`
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
		case options.Preconditions.ResourceVersion != "" && options.Preconditions.ResourceVersion != f.resourceVersion():
			return "", fmt.Errorf(`Error from server (Conflict): Operation cannot be fulfilled on `+
				`leases.coordination.k8s.io "campaign": Precondition failed: `+
				`ResourceVersion in precondition: %s, ResourceVersion in object meta: %s`,
				options.Preconditions.ResourceVersion, f.resourceVersion())
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
	f.version++
}

// update writes the Lease in place: same uid, new resourceVersion.
func (f *fakeCluster) update(spec map[string]any) {
	f.lease = spec
	f.version++
}

func (f *fakeCluster) resourceVersion() string { return fmt.Sprint(f.version) }

// spec is a Lease spec as another holder writes it; a zero renewed leaves
// renewTime out.
func spec(holder string, seconds int, acquired, renewed time.Time) map[string]any {
	s := map[string]any{"holderIdentity": holder, "leaseDurationSeconds": seconds, "acquireTime": acquired}
	if !renewed.IsZero() {
		s["renewTime"] = renewed
	}
	return s
}

const otherHolder = "other"

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

// killedRun is a holder in the form this entry point writes.
const killedRun = "alice@ws/4242/2026-10-15T06:00:00Z"

// A Lease of this entry point past its duration is reported, with the way to
// remove it, but not taken over.
func TestAnExpiredLeaseIsNotTakenOver(t *testing.T) {
	ctx := context.Background()
	f := &fakeCluster{context: labCtx, marker: labA}
	target := Target{Context: labCtx, Name: labA}
	if _, err := target.Acquire(ctx, f.kubectl, killedRun, time.Hour, t0); err != nil {
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
	if got := f.lease["holderIdentity"]; got != killedRun {
		t.Fatalf("the Lease holder changed to %v", got)
	}
}

// Another tool's Lease, such as the canonical launcher's 60-second Lease that
// is renewed every 10 seconds, is live as long as it is renewed, whenever it
// was acquired; and since only the tool that holds it knows whether its
// campaign still runs, the refusal never advises deleting it.
func TestAForeignLeaseIsNeverAdvisedForDeletion(t *testing.T) {
	ctx := context.Background()
	target := Target{Context: labCtx, Name: labA}
	acquired := t0
	for _, tc := range []struct {
		name    string
		renewed time.Time
		now     time.Time
	}{
		{"renewed and live", t0.Add(2 * time.Hour), t0.Add(2*time.Hour + 30*time.Second)},
		{"no longer renewed", t0.Add(time.Hour), t0.Add(3 * time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeCluster{context: labCtx, marker: labA}
			f.store(spec("operator-campaign-lease/9f2c1e", 60, acquired, tc.renewed))
			_, err := target.Acquire(ctx, f.kubectl, "mine", time.Hour, tc.now)
			if err == nil {
				t.Fatal("a foreign Lease was taken over")
			}
			if !strings.Contains(err.Error(), `held by "operator-campaign-lease/9f2c1e"`) {
				t.Fatalf("the refusal should name the holder: %v", err)
			}
			if strings.Contains(err.Error(), "delete lease") || strings.Contains(err.Error(), "may have been killed") {
				t.Fatalf("the refusal advises removing a Lease this entry point did not create: %v", err)
			}
		})
	}
}

// The end of a Lease is counted from its last renewal: a Lease of this entry
// point's form that its holder renewed is not past its end.
func TestARenewedLeaseIsNotPastItsEnd(t *testing.T) {
	f := &fakeCluster{context: labCtx, marker: labA}
	f.store(spec(killedRun, 3600, t0, t0.Add(3*time.Hour)))
	_, err := (Target{Context: labCtx, Name: labA}).Acquire(context.Background(), f.kubectl, "mine", time.Hour,
		t0.Add(3*time.Hour+time.Minute))
	if err == nil || strings.Contains(err.Error(), "delete lease") {
		t.Fatalf("Acquire() = %v, want a refusal without removal advice", err)
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

// A Lease that another holder wrote in place since the create, keeping its
// uid, is not this run's any more, and is not deleted.
func TestReleaseLeavesALeaseUpdatedInPlace(t *testing.T) {
	ctx := context.Background()
	f := &fakeCluster{context: labCtx, marker: labA}
	target := Target{Context: labCtx, Name: labA}
	for _, ambiguous := range []bool{false, true} {
		t.Run(fmt.Sprintf("ambiguous create=%v", ambiguous), func(t *testing.T) {
			f.lease = nil
			if ambiguous {
				f.createErr = errors.New("timeout")
			}
			lock, err := target.Acquire(ctx, f.kubectl, "mine", time.Hour, t0)
			if err != nil {
				t.Fatal(err)
			}
			f.update(spec(otherHolder, 60, t0, time.Time{}))
			if err := lock.Release(ctx, f.kubectl); err == nil || !strings.Contains(err.Error(), "left in place") {
				t.Fatalf("Release() = %v, want a refusal that leaves the updated Lease", err)
			}
			if f.lease == nil || f.lease["holderIdentity"] != otherHolder {
				t.Fatal("the Lease another holder updated in place was deleted")
			}
		})
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
			other.store(spec(otherHolder, 3600, t0, time.Time{}))
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

// A renewal writes the Lease in place, as this run holds it: only its
// renewTime changes, and only while nobody else has written it. The
// resourceVersion it returns is the one the next renewal and the release
// carry.
func TestRenewKeepsTheLeaseAndCarriesItsVersion(t *testing.T) {
	ctx := context.Background()
	f := &fakeCluster{context: labCtx, marker: labA}
	lock, err := (Target{Context: labCtx, Name: labA}).Acquire(ctx, f.kubectl, "mine", time.Minute, t0)
	if err != nil {
		t.Fatal(err)
	}
	acquired := f.lease["acquireTime"]
	for i := 1; i <= 2; i++ {
		now := t0.Add(time.Duration(i) * 10 * time.Second)
		if err := lock.Renew(ctx, f.kubectl, now); err != nil {
			t.Fatalf("renewal %d: %v", i, err)
		}
		if got := f.resourceVersion(); got != fmt.Sprint(1+i) {
			t.Fatalf("after renewal %d the Lease has resourceVersion %s, want %d", i, got, 1+i)
		}
		want := map[string]any{"holderIdentity": "mine", "leaseDurationSeconds": float64(60),
			"acquireTime": acquired, "renewTime": now.Format("2006-01-02T15:04:05.000000Z")}
		for key, value := range want {
			if f.lease[key] != value {
				t.Fatalf("after renewal %d the Lease has %s %v, want %v", i, key, f.lease[key], value)
			}
		}
	}
	if err := lock.Release(ctx, f.kubectl); err != nil {
		t.Fatalf("the release after the renewals: %v", err)
	}
	if f.lease != nil {
		t.Fatal("the Lease is still there after the release")
	}
}

// A Lease that was removed, or that someone else wrote, is lost: a renewal
// says so, and does not write it.
func TestRenewOfALeaseNoLongerThisRunsIsLost(t *testing.T) {
	ctx := context.Background()
	target := Target{Context: labCtx, Name: labA}
	for _, tc := range []struct {
		name   string
		change func(f *fakeCluster)
	}{
		{"removed", func(f *fakeCluster) { f.lease = nil }},
		{"written by another holder", func(f *fakeCluster) { f.update(spec(otherHolder, 60, t0, time.Time{})) }},
		{"removed and created again", func(f *fakeCluster) { f.store(spec(otherHolder, 60, t0, time.Time{})) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeCluster{context: labCtx, marker: labA}
			lock, err := target.Acquire(ctx, f.kubectl, "mine", time.Minute, t0)
			if err != nil {
				t.Fatal(err)
			}
			tc.change(f)
			before := f.resourceVersion()
			if err := lock.Renew(ctx, f.kubectl, t0.Add(10*time.Second)); !errors.Is(err, ErrLost) {
				t.Fatalf("Renew() = %v, want ErrLost", err)
			}
			if f.resourceVersion() != before {
				t.Fatal("a Lease that is no longer this run's was written")
			}
		})
	}

	// Any other failure may pass: it is not a loss by itself.
	f := &fakeCluster{context: labCtx, marker: labA}
	lock, err := target.Acquire(ctx, f.kubectl, "mine", time.Minute, t0)
	if err != nil {
		t.Fatal(err)
	}
	down := func(context.Context, string, ...string) (string, error) {
		return "", errors.New("Unable to connect to the server: dial tcp: i/o timeout")
	}
	if err := lock.Renew(ctx, down, t0.Add(10*time.Second)); err == nil || errors.Is(err, ErrLost) {
		t.Fatalf("Renew() = %v, want an error that is not ErrLost", err)
	}
	if err := lock.Renew(ctx, f.kubectl, t0.Add(20*time.Second)); err != nil {
		t.Fatalf("the renewal after the failed one: %v", err)
	}
}

// A renewal the API server stored although kubectl failed, as after a
// timeout, leaves the run a stale resourceVersion. The Conflict that follows
// is not a loss when the Lease still carries this run's uid, holder and a
// renewTime it sent: the run takes the Lease's resourceVersion and goes on,
// and so does a release.
func TestAStoredButFailedRenewalIsNotALoss(t *testing.T) {
	ctx := context.Background()
	target := Target{Context: labCtx, Name: labA}
	timeout := errors.New("Unable to connect to the server: net/http: request canceled (Client.Timeout exceeded)")

	f := &fakeCluster{context: labCtx, marker: labA}
	lock, err := target.Acquire(ctx, f.kubectl, "mine", time.Minute, t0)
	if err != nil {
		t.Fatal(err)
	}
	f.renewErr = timeout
	if err := lock.Renew(ctx, f.kubectl, t0.Add(10*time.Second)); err == nil || errors.Is(err, ErrLost) {
		t.Fatalf("Renew() = %v, want an error that is not ErrLost", err)
	}
	now := t0.Add(20 * time.Second)
	if err := lock.Renew(ctx, f.kubectl, now); err != nil {
		t.Fatalf("the renewal after a stored but failed one: %v", err)
	}
	if got := f.lease["renewTime"]; got != now.Format(microTime) {
		t.Fatalf("the Lease has renewTime %v, want %s", got, now.Format(microTime))
	}
	if err := lock.Release(ctx, f.kubectl); err != nil || f.lease != nil {
		t.Fatalf("Release() = %v, Lease %v; want it deleted", err, f.lease)
	}

	// The release right after such a renewal deletes the Lease too.
	f = &fakeCluster{context: labCtx, marker: labA}
	if lock, err = target.Acquire(ctx, f.kubectl, "mine", time.Minute, t0); err != nil {
		t.Fatal(err)
	}
	f.renewErr = timeout
	_ = lock.Renew(ctx, f.kubectl, t0.Add(10*time.Second))
	if err := lock.Release(ctx, f.kubectl); err != nil || f.lease != nil {
		t.Fatalf("Release() after a stored but failed renewal = %v, Lease %v; want it deleted", err, f.lease)
	}
}

// After a stored but failed renewal, a Lease that someone else wrote since,
// or that is gone, is still lost, and a release leaves it alone.
func TestAStoredButFailedRenewalDoesNotHideALoss(t *testing.T) {
	ctx := context.Background()
	target := Target{Context: labCtx, Name: labA}
	for _, tc := range []struct {
		name   string
		change func(f *fakeCluster)
	}{
		{"written by another holder", func(f *fakeCluster) { f.update(spec(otherHolder, 60, t0, t0)) }},
		{"written with this holder but not by this run", func(f *fakeCluster) {
			f.update(spec("mine", 60, t0, t0.Add(time.Hour)))
		}},
		{"removed", func(f *fakeCluster) { f.lease = nil }},
		{"removed and created again", func(f *fakeCluster) { f.store(spec("mine", 60, t0, t0.Add(10*time.Second))) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeCluster{context: labCtx, marker: labA}
			lock, err := target.Acquire(ctx, f.kubectl, "mine", time.Minute, t0)
			if err != nil {
				t.Fatal(err)
			}
			f.renewErr = errors.New("timeout")
			_ = lock.Renew(ctx, f.kubectl, t0.Add(10*time.Second))
			tc.change(f)
			before, lease := f.resourceVersion(), f.lease
			if err := lock.Renew(ctx, f.kubectl, t0.Add(20*time.Second)); !errors.Is(err, ErrLost) {
				t.Fatalf("Renew() = %v, want ErrLost", err)
			}
			if err := lock.Release(ctx, f.kubectl); err == nil {
				t.Fatal("the release of a lost Lease succeeded")
			}
			if f.resourceVersion() != before || (lease != nil && f.lease == nil) {
				t.Fatal("a Lease that is no longer this run's was written or deleted")
			}
		})
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

// The holders this entry point writes are the ones it recognizes as its own.
func TestHolderIsRecognizedAsOwn(t *testing.T) {
	if got := Holder("alice", "ws", 4242, t0); got != killedRun {
		t.Fatalf("Holder() = %q, want %q", got, killedRun)
	}
	if h := Holder("ci-runner", "lab-host.example", 1, time.Now()); !ownHolder.MatchString(h) {
		t.Fatalf("the holder %q is not recognized as this entry point's", h)
	}
}
