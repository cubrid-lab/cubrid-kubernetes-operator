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
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
)

// imAnswer is what the client makes of an Instance Manager answer with
// status and body to a start.
func imAnswer(status int, body string) error {
	return startError(&http.Response{StatusCode: status, Header: http.Header{},
		Body: io.NopCloser(strings.NewReader(body))})
}

const (
	busyBody     = `{"error":"a mutating operation is already in progress for this database","code":"OperationInProgress"}`
	conflictBody = `{"error":"idempotency key reused with a different request","code":"IdempotencyConflict"}`
)

// startCalls are the three starts of the client, each with the same answer.
func startCalls(c *HTTPBackupClient) map[string]func() error {
	ctx := context.Background()
	return map[string]func() error{
		"StartBackup": func() error {
			_, err := c.StartBackup(ctx, "db-0", "ns", "k", instancemanager.BackupRequest{})
			return err
		},
		"StartRestore": func() error {
			_, err := c.StartRestore(ctx, "db-0", "ns", "k", instancemanager.RestoreRequest{})
			return err
		},
		"StartHABootstrap": func() error {
			_, err := c.StartHABootstrap(ctx, "db-0", "ns", "k", instancemanager.HABootstrapRequest{})
			return err
		},
	}
}

// Each start tells the causes of a refusal apart by the answer's code, keeps
// the manager's message, and falls back to the HTTP status for a manager
// that sends no code (#302).
func TestHTTPBackupClient_ErrorAnswersAreClassified(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		is     []error
		isNot  []error
		reason string
		says   string
	}{
		{"operation in progress", http.StatusConflict, busyBody,
			[]error{errStartConflict, errOperationInProgress}, []error{errRequestConflict, errStagingInUse},
			reasonOperationInProgress, "already in progress"},
		{"key held for another request", http.StatusConflict, conflictBody,
			[]error{errStartConflict, errRequestConflict}, []error{errOperationInProgress, errStagingInUse},
			reasonRequestConflict, "idempotency key reused with a different request"},
		{"staging destination in use", http.StatusConflict,
			`{"error":"the backup staging destination is owned by another operation that is still running: bk is owned by operation op-1","code":"StagingDestinationInUse"}`,
			[]error{errStartConflict, errStagingInUse}, []error{errOperationInProgress, errRequestConflict},
			reasonStagingInUse, "owned by operation op-1"},
		{"a member that is no HA member", http.StatusConflict,
			`{"error":"this member is a standalone server, not an HA member","code":"NotAnHAMember"}`,
			[]error{errStartConflict, errNotAnHAMember}, []error{errOperationInProgress, errRequestConflict, errStagingInUse},
			reasonNotAnHAMember, "standalone server"},
		{"a conflict of a code this operator does not know", http.StatusConflict,
			`{"error":"a cause named by a later manager","code":"SomethingLater"}`,
			[]error{errStartConflict}, []error{errOperationInProgress, errRequestConflict, errStagingInUse, errNotAnHAMember},
			reasonConflict, "a cause named by a later manager"},
		{"an earlier manager's conflict, without a code", http.StatusConflict,
			`{"error":"idempotency key reused with a different request"}`,
			[]error{errStartConflict}, []error{errOperationInProgress, errRequestConflict, errStagingInUse},
			reasonConflict, "idempotency key reused with a different request"},
		{"no object storage", http.StatusPreconditionFailed,
			`{"error":"this Instance Manager was started without object storage","code":"ObjectStorageNotApplied"}`,
			[]error{errObjectStorageNotApplied}, []error{errStartConflict}, reasonObjectStorageNotApplied, "without object storage"},
		{"an earlier manager without object storage", http.StatusPreconditionFailed, `{"error":"x"}`,
			[]error{errObjectStorageNotApplied}, []error{errStartConflict}, reasonObjectStorageNotApplied, ""},
		{"a refused token", http.StatusUnauthorized, `{"error":"unauthorized","code":"Unauthorized"}`,
			[]error{errTokenRefused}, []error{errStartConflict}, reasonTokenRefused, "refused the cluster's Instance Manager token"},
		{"an operation record the member cannot read", http.StatusInternalServerError,
			`{"error":"an operation record cannot be read: decode operation op-1: unexpected end of JSON input","code":"OperationRecordUnreadable"}`,
			[]error{errOperationRecordUnreadable}, []error{errStartConflict, errTokenRefused, errRequestRefused},
			reasonOperationRecordUnreadable, "decode operation op-1"},
		{"a server error", http.StatusInternalServerError, `{"error":"write operation record: disk full"}`,
			nil, []error{errStartConflict, errTokenRefused, errRequestRefused}, "InstanceManagerUnavailable", "disk full"},
		{"a request the member refuses as invalid", http.StatusBadRequest, `{"error":"database name \"a b\" is not a plain identifier"}`,
			[]error{errRequestRefused}, []error{errStartConflict, errTokenRefused}, reasonRequestRefused, "not a plain identifier"},
		{"a request body that is too large", http.StatusRequestEntityTooLarge, `{"error":"the request body is larger than 16384 bytes"}`,
			[]error{errRequestRefused}, []error{errStartConflict, errTokenRefused}, reasonRequestRefused, "larger than"},
		{"a request the member cannot process", http.StatusUnprocessableEntity, `{"error":"unprocessable"}`,
			[]error{errRequestRefused}, []error{errStartConflict, errTokenRefused}, reasonRequestRefused, "unprocessable"},
		// An earlier manager without the route, or one that cannot take the
		// request now, could not be asked: the start waits.
		{"an endpoint an earlier member does not have", http.StatusNotFound, `404 page not found`,
			nil, []error{errRequestRefused, errStartConflict, errTokenRefused}, reasonInstanceManagerUnavailable, ""},
		{"a method an earlier member does not have", http.StatusMethodNotAllowed, `Method Not Allowed`,
			nil, []error{errRequestRefused, errStartConflict}, reasonInstanceManagerUnavailable, ""},
		{"a request that timed out", http.StatusRequestTimeout, ``,
			nil, []error{errRequestRefused, errStartConflict}, reasonInstanceManagerUnavailable, ""},
		{"too many requests", http.StatusTooManyRequests, `{"error":"slow down"}`,
			nil, []error{errRequestRefused, errStartConflict}, reasonInstanceManagerUnavailable, "slow down"},
	} {
		c := NewHTTPBackupClient(staticToken("tok"))
		c.Client = &http.Client{Transport: &answerBody{status: tc.status, body: tc.body}}
		for name, call := range startCalls(c) {
			err := call()
			for _, want := range tc.is {
				if !errors.Is(err, want) {
					t.Errorf("%s, %s: err = %v, want it to be %v", tc.name, name, err, want)
				}
			}
			for _, not := range tc.isNot {
				if errors.Is(err, not) {
					t.Errorf("%s, %s: err = %v, must not be %v", tc.name, name, err, not)
				}
			}
			if got := instanceManagerFailureReason(err); got != tc.reason {
				t.Errorf("%s, %s: reason = %q, want %q", tc.name, name, got, tc.reason)
			}
			if tc.says != "" && !strings.Contains(err.Error(), tc.says) {
				t.Errorf("%s, %s: %q does not carry the manager's message %q", tc.name, name, err, tc.says)
			}
		}
	}
}

// A poll that the member answers 4xx is not a refused request: only a start
// is refused as one.
func TestHTTPBackupClient_PollIsNotARefusedRequest(t *testing.T) {
	c := NewHTTPBackupClient(staticToken("tok"))
	c.Client = &http.Client{Transport: &answerBody{status: http.StatusNotFound, body: `{"error":"operation not found"}`}}
	_, err := c.GetOperation(context.Background(), "db-0", "ns", "op-1")
	if errors.Is(err, errRequestRefused) || instanceManagerFailureReason(err) != reasonInstanceManagerUnavailable {
		t.Errorf("a poll answered 404: err = %v (reason %s), want an unavailable manager", err, instanceManagerFailureReason(err))
	}
}

// What the lookup after a conflict answers decides the reason when it is a
// refused token, and a member that is no HA member is not asked for the key.
func TestOperationOfKey(t *testing.T) {
	ctx := context.Background()
	conflict := imAnswer(http.StatusConflict, conflictBody)
	refused := imAnswer(http.StatusUnauthorized, `{"error":"unauthorized","code":"Unauthorized"}`)
	_, err := operationOfKey(ctx, conflict, instancemanager.OpRestore, "k", func() (instancemanager.Operation, bool, error) {
		return instancemanager.Operation{}, false, refused
	})
	if got := instanceManagerFailureReason(err); got != reasonTokenRefused {
		t.Errorf("a lookup the member refused the token for: reason = %q (%v), want %q", got, err, reasonTokenRefused)
	}

	asked := false
	notHA := imAnswer(http.StatusConflict, `{"error":"this member is a standalone server, not an HA member","code":"NotAnHAMember"}`)
	_, err = operationOfKey(ctx, notHA, instancemanager.OpHABootstrap, "k", func() (instancemanager.Operation, bool, error) {
		asked = true
		return instancemanager.Operation{}, false, nil
	})
	if asked || !errors.Is(err, errNotAnHAMember) || instanceManagerFailureReason(err) != reasonNotAnHAMember {
		t.Errorf("a member that is no HA member: asked = %v, err = %v (reason %s), want no lookup and %s",
			asked, err, instanceManagerFailureReason(err), reasonNotAnHAMember)
	}
}

// countingBody counts what is read of it.
type countingBody struct {
	r    io.Reader
	read int
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.read += n
	return n, err
}

func (b *countingBody) Close() error { return nil }

// What the manager sends with a refusal goes into conditions and Events: it
// is read only up to a bound, its message is cut to a length, and it carries
// no control characters. A body that is no error answer adds nothing.
func TestHTTPBackupClient_ErrorBodyIsBoundedAndSanitized(t *testing.T) {
	// A body larger than the bound is kept only up to the bound and drained
	// only up to another one, and the status decides.
	huge := `{"error":"` + strings.Repeat("x", 1<<20) + `","code":"OperationInProgress"}`
	body := &countingBody{r: strings.NewReader(huge)}
	err := unexpectedStatus(&http.Response{StatusCode: http.StatusConflict, Header: http.Header{}, Body: body})
	if body.read > maxErrorBody+1+maxErrorDrain {
		t.Errorf("read %d bytes of the body, want at most %d", body.read, maxErrorBody+1+maxErrorDrain)
	}
	if !errors.Is(err, errStartConflict) || len(err.Error()) > maxErrorMessage+200 {
		t.Errorf("an oversized body: err = %.300v, want the conflict of the status", err)
	}

	// A long message with control characters, within the bound, is cut and
	// cleaned; a code that is not a plain word is not taken as a cause.
	long := `{"error":"line one\n\u001b[31mred\u001b[0m\t\u202e` + strings.Repeat("x", 3000) + `","code":"Operation\nInProgress"}`
	err = imAnswer(http.StatusConflict, long)
	msg := err.Error()
	if len(msg) > maxErrorMessage+200 {
		t.Errorf("error is %d bytes long, want the message cut to about %d", len(msg), maxErrorMessage)
	}
	if !strings.Contains(msg, "line one") {
		t.Errorf("%q lost the start of the message", msg)
	}
	for _, r := range msg {
		if !unicode.IsPrint(r) {
			t.Errorf("%q carries the control character %U", msg, r)
		}
	}
	if errors.Is(err, errOperationInProgress) {
		t.Errorf("a malformed code was taken as a cause: %v", err)
	}
	if !errors.Is(err, errStartConflict) {
		t.Errorf("err = %v, want the conflict of the status", err)
	}

	for _, notAnAnswer := range []string{"<html>proxy error</html>", "", `["x"]`, `{"error":42}`} {
		err := imAnswer(http.StatusConflict, notAnAnswer)
		if !errors.Is(err, errStartConflict) || instanceManagerFailureReason(err) != reasonConflict {
			t.Errorf("body %q: err = %v, want the conflict of the status", notAnAnswer, err)
		}
		if strings.Contains(err.Error(), "proxy") {
			t.Errorf("body %q: a body that is no error answer went into the error: %v", notAnAnswer, err)
		}
	}
}

// nopObjects is an object store that holds nothing; the requests of these
// tests are refused before it is used.
type nopObjects struct{}

func (nopObjects) Put(context.Context, string, string, io.Reader, int64, string) (int64, error) {
	return 0, errors.New("not used")
}
func (nopObjects) StatSize(context.Context, string, string) (int64, error) {
	return 0, errors.New("not used")
}
func (nopObjects) Get(context.Context, string, string) (io.ReadCloser, error) {
	return nil, errors.New("not used")
}

// toHandler sends every call to an Instance Manager handler in the process.
type toHandler struct{ h http.Handler }

func (t toHandler) RoundTrip(req *http.Request) (*http.Response, error) {
	req.RemoteAddr = "10.0.0.1:5000"
	rr := httptest.NewRecorder()
	t.h.ServeHTTP(rr, req)
	return rr.Result(), nil
}

// The client and the Instance Manager of this version agree on the codes: a
// key sent again with another request is told apart from a database that
// another operation holds, and the operation the key names is found (#302).
func TestHTTPBackupClient_AgainstTheInstanceManager(t *testing.T) {
	store, err := instancemanager.NewOperationStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := instancemanager.NewServer(nil, "tok").WithOperationStore(store).WithObjectStore(nopObjects{}).Handler()
	held, _, err := store.FindOrCreate(instancemanager.OpRestore, "restore-key", "hash-of-the-first-request", testDatabase)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = store.Update(held.ID, func(o *instancemanager.Operation) { o.State = instancemanager.OpFailed })
	}()
	c := NewHTTPBackupClient(staticToken("tok"))
	c.Client = &http.Client{Transport: toHandler{h}}
	ctx := context.Background()
	req := instancemanager.RestoreRequest{Database: testDatabase, Bucket: "b", Prefix: "p", ExpectedCubridVersion: "11.4",
		StagingDir: "/var/lib/cubrid/restore-staging/uid", TargetDir: restoreTargetRoot}

	_, err = c.StartRestore(ctx, "db-0", "ns", "restore-key", req)
	if !errors.Is(err, errRequestConflict) || instanceManagerFailureReason(err) != reasonRequestConflict {
		t.Errorf("same key, another request: err = %v (reason %s), want a request conflict", err, instanceManagerFailureReason(err))
	}
	op, found, err := c.FindOperation(ctx, "db-0", "ns", instancemanager.OpRestore, "restore-key")
	if err != nil || !found || op.ID != held.ID {
		t.Errorf("FindOperation = %+v, %v, %v; want the held operation %s", op, found, err, held.ID)
	}

	_, err = c.StartRestore(ctx, "db-0", "ns", "another-key", req)
	if !errors.Is(err, errOperationInProgress) || instanceManagerFailureReason(err) != reasonOperationInProgress {
		t.Errorf("another key while the database is held: err = %v, want an operation in progress", err)
	}

	refused := NewHTTPBackupClient(staticToken("not-the-token"))
	refused.Client = &http.Client{Transport: toHandler{h}}
	if _, err := refused.StartRestore(ctx, "db-0", "ns", "restore-key", req); !errors.Is(err, errTokenRefused) ||
		errors.Is(err, errStartConflict) {
		t.Errorf("a refused token: err = %v, want a token refusal and nothing else", err)
	}
}

// A start that the real Instance Manager refuses because its operation store
// holds a record it cannot read is reported by its own reason, retryable and
// with a pointer to the runbook, on every kind of start (#481).
func TestHTTPBackupClient_UnreadableRecordAgainstTheInstanceManager(t *testing.T) {
	const member = "db-0"
	dir := t.TempDir()
	corrupt := filepath.Join(dir, "op-"+strings.Repeat("0", 32)+".json")
	if err := os.WriteFile(corrupt, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := instancemanager.NewOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	staging := t.TempDir()
	h := instancemanager.NewServer(nil, "tok").WithOperationStore(store).WithObjectStore(nopObjects{}).
		WithBackupStagingRoot(staging).Handler()
	c := NewHTTPBackupClient(staticToken("tok"))
	c.Client = &http.Client{Transport: toHandler{h}}
	ctx := context.Background()
	starts := map[string]func() error{
		"backup": func() error {
			_, err := c.StartBackup(ctx, member, "ns", "backup-key", instancemanager.BackupRequest{
				Database: testDatabase, Destination: filepath.Join(staging, "uid"),
				Upload: &instancemanager.BackupUpload{Bucket: "b", Prefix: "p", ClusterUID: "c", CubridVersion: cubridVersion,
					SourceInstance: member, SourceRole: string(instancemanager.RoleMaster)}})
			return err
		},
		"restore": func() error {
			_, err := c.StartRestore(ctx, member, "ns", "restore-key", instancemanager.RestoreRequest{
				Database: testDatabase, Bucket: "b", Prefix: "p", ExpectedCubridVersion: cubridVersion,
				StagingDir: "/var/lib/cubrid/restore-staging/uid", TargetDir: restoreTargetRoot})
			return err
		},
	}
	for name, start := range starts {
		err := start()
		if !errors.Is(err, errOperationRecordUnreadable) || errors.Is(err, errRequestRefused) || errors.Is(err, errStartConflict) {
			t.Errorf("%s: err = %v, want only an unreadable operation record", name, err)
		}
		if got := instanceManagerFailureReason(err); got != reasonOperationRecordUnreadable {
			t.Errorf("%s: reason = %q, want %q", name, got, reasonOperationRecordUnreadable)
		}
		if err != nil && !strings.Contains(err.Error(), "runbook") {
			t.Errorf("%s: %q does not point at the runbook", name, err)
		}
	}
}

var _ = Describe("Terminal and retryable answers of a recovery restore (#302)", func() {
	ctx := context.Background()

	newRecovery := func(name string) types.NamespacedName {
		c := standaloneCluster(name)
		c.Spec.Bootstrap = &databasev1alpha1.CubridBootstrap{
			Recovery: &databasev1alpha1.RecoverySource{ManifestURI: testManifestURI},
		}
		c.Spec.ObjectStorage = haObjectStorage()
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, c) })
		return types.NamespacedName{Name: name, Namespace: c.Namespace}
	}
	reconcileOnce := func(r *CubridClusterReconciler, key types.NamespacedName) (reconcile.Result, *databasev1alpha1.CubridCluster) {
		GinkgoHelper()
		res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		got := &databasev1alpha1.CubridCluster{}
		Expect(k8sClient.Get(ctx, key, got)).To(Succeed())
		Expect(meta.IsStatusConditionTrue(got.Status.Conditions, conditionReady)).To(BeFalse())
		return res, got
	}
	bootstrapReady := func(c *databasev1alpha1.CubridCluster) string {
		GinkgoHelper()
		cond := meta.FindStatusCondition(c.Status.Conditions, conditionBootstrapReady)
		Expect(cond).NotTo(BeNil())
		return cond.Reason
	}

	It("keeps the reason of a failed recovery and does not ask the member again", func() {
		restore := &fakeRestoreClient{}
		r := &CubridClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Restore: restore}
		key := newRecovery("recovery-terminal")
		var got *databasev1alpha1.CubridCluster
		for range maxBootstrapAttempts {
			restore.failed = false
			reconcileOnce(r, key)
			restore.failed = true
			_, got = reconcileOnce(r, key)
		}
		Expect(got.Status.Bootstrap.Phase).To(Equal(databasev1alpha1.BootstrapFailed))
		Expect(bootstrapReady(got)).To(Equal("RestoreFailed"))
		message := meta.FindStatusCondition(got.Status.Conditions, conditionBootstrapReady).Message

		By("the cluster reporting the recovery as failed, not in progress (#434)")
		ready := meta.FindStatusCondition(got.Status.Conditions, conditionReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal(reasonBootstrapRecoveryFailed))
		Expect(ready.Message).To(ContainSubstring(message))
		progressing := meta.FindStatusCondition(got.Status.Conditions, conditionProgressing)
		Expect(progressing).NotTo(BeNil())
		Expect(progressing.Status).To(Equal(metav1.ConditionFalse), "a failed recovery is not in progress")
		Expect(progressing.Reason).To(Equal(reasonBootstrapRecoveryFailed))

		By("the member becoming unreachable after the recovery failed")
		polls := restore.polls
		restore.pollErr = errors.New("dial tcp: connection refused")
		for range 2 {
			res, got := reconcileOnce(r, key)
			Expect(res.RequeueAfter).To(BeZero(), "a failed recovery is not polled again")
			Expect(got.Status.Bootstrap.Phase).To(Equal(databasev1alpha1.BootstrapFailed))
			Expect(bootstrapReady(got)).To(Equal("RestoreFailed"))
			Expect(meta.FindStatusCondition(got.Status.Conditions, conditionBootstrapReady).Message).To(Equal(message))
		}
		Expect(restore.polls).To(Equal(polls), "the operation of a failed recovery was polled again")
		Expect(restore.keys).To(HaveLen(maxBootstrapAttempts))
	})

	It("waits while another operation holds the database, and then starts", func() {
		restore := &fakeRestoreClient{startErr: imAnswer(http.StatusConflict, busyBody)}
		r := &CubridClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Restore: restore}
		key := newRecovery("recovery-busy")
		res, got := reconcileOnce(r, key)
		Expect(res.RequeueAfter).To(BeNumerically(">", 0))
		Expect(bootstrapReady(got)).To(Equal(reasonOperationInProgress))
		Expect(got.Status.Bootstrap.Phase).NotTo(Equal(databasev1alpha1.BootstrapFailed))
		Expect(got.Status.Bootstrap.Attempts).To(BeZero(), "waiting uses up no attempt")
		Expect(restore.lookups).To(BeZero(), "a busy member holds nothing under the key")

		restore.startErr = nil
		_, got = reconcileOnce(r, key)
		Expect(got.Status.Bootstrap.OperationID).To(Equal(opRestore))
		Expect(restore.keys).To(HaveLen(2))
		Expect(restore.keys[1]).To(Equal(restore.keys[0]))
	})

	It("names an unreadable operation record, waits and then starts (#481)", func() {
		restore := &fakeRestoreClient{startErr: imAnswer(http.StatusInternalServerError,
			`{"error":"an operation record cannot be read: decode operation op-1: unexpected end of JSON input","code":"OperationRecordUnreadable"}`)}
		r := &CubridClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Restore: restore}
		key := newRecovery("recovery-unreadable")
		for range maxBootstrapAttempts + 1 {
			res, got := reconcileOnce(r, key)
			Expect(res.RequeueAfter).To(BeNumerically(">", 0))
			Expect(bootstrapReady(got)).To(Equal(reasonOperationRecordUnreadable))
			Expect(meta.FindStatusCondition(got.Status.Conditions, conditionBootstrapReady).Message).To(
				ContainSubstring("Runbook: an operation record that cannot be read"))
			Expect(got.Status.Bootstrap.Phase).NotTo(Equal(databasev1alpha1.BootstrapFailed))
			Expect(got.Status.Bootstrap.Attempts).To(BeZero(), "waiting uses up no attempt")
		}
		Expect(restore.lookups).To(BeZero(), "a member that cannot read its records is not asked for the key")

		restore.startErr = nil
		_, got := reconcileOnce(r, key)
		Expect(got.Status.Bootstrap.OperationID).To(Equal(opRestore))
	})

	It("follows the operation its key names when the member holds it for another request", func() {
		restore := &fakeRestoreClient{
			startErr: imAnswer(http.StatusConflict, conflictBody),
			held:     &instancemanager.Operation{ID: "op-first-request", Kind: instancemanager.OpRestore, State: instancemanager.OpRestoring},
		}
		r := &CubridClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Restore: restore}
		key := newRecovery("recovery-held")
		_, got := reconcileOnce(r, key)
		Expect(got.Status.Bootstrap.OperationID).To(Equal("op-first-request"))
		Expect(got.Status.Bootstrap.Phase).To(Equal(databasev1alpha1.BootstrapRestoring))
		Expect(restore.lookups).To(Equal(1))

		reconcileOnce(r, key)
		Expect(restore.keys).To(HaveLen(1), "the request was sent again after the operation of its key was known")
		Expect(restore.polls).To(Equal(1))
	})

	It("names the conflict when the operation its key names cannot be found", func() {
		restore := &fakeRestoreClient{startErr: imAnswer(http.StatusConflict, conflictBody)}
		r := &CubridClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Restore: restore}
		key := newRecovery("recovery-conflict")
		res, got := reconcileOnce(r, key)
		Expect(res.RequeueAfter).To(BeNumerically(">", 0))
		Expect(bootstrapReady(got)).To(Equal(reasonRequestConflict))
		cond := meta.FindStatusCondition(got.Status.Conditions, conditionBootstrapReady)
		Expect(cond.Message).To(ContainSubstring("idempotency key reused with a different request"))
		Expect(cond.Message).To(ContainSubstring("holds no restore operation under the key"))
		Expect(got.Status.Bootstrap.OperationID).To(BeEmpty())
		Expect(got.Status.Bootstrap.Phase).NotTo(Equal(databasev1alpha1.BootstrapFailed))
	})

	// An Instance Manager image without the route answers 404 from its mux:
	// it could not be asked, which uses up no attempt.
	It("waits on a member that has no restore endpoint, without using up an attempt", func() {
		restore := &fakeRestoreClient{startErr: imAnswer(http.StatusNotFound, "404 page not found")}
		r := &CubridClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Restore: restore}
		key := newRecovery("recovery-no-route")
		var got *databasev1alpha1.CubridCluster
		var res reconcile.Result
		for range maxBootstrapAttempts + 1 {
			res, got = reconcileOnce(r, key)
		}
		Expect(res.RequeueAfter).To(BeNumerically(">", 0))
		Expect(got.Status.Bootstrap.Phase).NotTo(Equal(databasev1alpha1.BootstrapFailed))
		Expect(got.Status.Bootstrap.Attempts).To(BeZero())
		Expect(bootstrapReady(got)).To(Equal("RestoreStartFailed"))
		Expect(restore.keys).To(HaveLen(maxBootstrapAttempts + 1))
		Expect(restore.keys).To(HaveEach(restore.keys[0]))
	})

	It("counts a restore the member refuses as invalid as a failed attempt, and ends", func() {
		restore := &fakeRestoreClient{startErr: imAnswer(http.StatusBadRequest, `{"error":"database name is not a plain identifier"}`)}
		r := &CubridClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Restore: restore}
		key := newRecovery("recovery-invalid")
		var got *databasev1alpha1.CubridCluster
		for range maxBootstrapAttempts {
			_, got = reconcileOnce(r, key)
		}
		Expect(got.Status.Bootstrap.Phase).To(Equal(databasev1alpha1.BootstrapFailed))
		Expect(bootstrapReady(got)).To(Equal("RestoreFailed"))
		Expect(meta.FindStatusCondition(got.Status.Conditions, conditionBootstrapReady).Message).To(
			ContainSubstring("not a plain identifier"))
		Expect(restore.keys).To(HaveLen(maxBootstrapAttempts))
		reconcileOnce(r, key)
		Expect(restore.keys).To(HaveLen(maxBootstrapAttempts), "a refused request was sent again after the recovery ended")
		Expect(restore.lookups).To(BeZero())
	})

	It("tells a refused token apart at the start", func() {
		restore := &fakeRestoreClient{startErr: imAnswer(http.StatusUnauthorized, `{"error":"unauthorized","code":"Unauthorized"}`)}
		r := &CubridClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Restore: restore}
		key := newRecovery("recovery-refused")
		_, got := reconcileOnce(r, key)
		Expect(bootstrapReady(got)).To(Equal(reasonTokenRefused))
		Expect(restore.lookups).To(BeZero())
	})
})

// A seed restore retry takes no new seed backup only when the earlier restore
// was started with the same request body, which the operator compares by the
// hash the Instance Manager keeps (#463). The Instance Manager of this version
// must therefore hash exactly what the client sends: a restore held under a
// key with the operator's hash of a request answers that request as the same,
// and a request with one field changed as another. This guards that contract
// between the operator and the Instance Manager; it does not reproduce the
// unused seed backup, which the seeding specs show.
func TestHTTPBackupClient_RequestHashAgreesWithTheInstanceManager(t *testing.T) {
	store, err := instancemanager.NewOperationStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	req := instancemanager.RestoreRequest{Database: testDatabase, Bucket: "b", Prefix: "p/uid/seed/" + testDatabase,
		ExpectedCubridVersion: "11.4", StagingDir: "/var/lib/cubrid/restore-staging/seed-uid", TargetDir: restoreTargetRoot,
		SeedFromMaster: "db-0"}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	held, _, err := store.FindOrCreate(instancemanager.OpRestore, "seed-restore-key", instancemanager.HashRequest(body), testDatabase)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(held.ID, func(o *instancemanager.Operation) { o.State = instancemanager.OpFailed }); err != nil {
		t.Fatal(err)
	}
	h := instancemanager.NewServer(nil, "tok").WithOperationStore(store).WithObjectStore(nopObjects{}).Handler()
	c := NewHTTPBackupClient(staticToken("tok"))
	c.Client = &http.Client{Transport: toHandler{h}}
	op, err := c.StartRestore(context.Background(), "db-1", "ns", "seed-restore-key", req)
	if err != nil || op.ID != held.ID || op.RequestHash != held.RequestHash {
		t.Fatalf("StartRestore = %+v, %v; want the held operation %s with the operator's hash of the request", op, err, held.ID)
	}
	changed := req
	changed.ExpectedCubridVersion = "11.4.1"
	if _, err := c.StartRestore(context.Background(), "db-1", "ns", "seed-restore-key", changed); !errors.Is(err, errRequestConflict) {
		t.Errorf("StartRestore with a changed field: err = %v, want a request conflict (another hash)", err)
	}
}
