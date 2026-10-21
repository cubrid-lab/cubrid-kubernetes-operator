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
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
)

// BackupClient drives one instance's Instance Manager backup operations
// (ADR-0007). It is an interface so the reconciler can be tested without a live
// manager.
type BackupClient interface {
	// StartBackup POSTs /v1/backup with the idempotency key and returns the
	// durable operation. A repeat with the same key returns the same operation.
	StartBackup(ctx context.Context, podName, namespace, idempotencyKey string, req instancemanager.BackupRequest) (instancemanager.Operation, error)
	// GetOperation polls /v1/operations/{id}.
	GetOperation(ctx context.Context, podName, namespace, id string) (instancemanager.Operation, error)
	// FindBackupOperation asks GET /v1/operations?idempotencyKey= for the
	// backup operation the member holds under the key. It never starts
	// anything. found is false only when the member answered that it holds
	// none; any other answer is an error.
	FindBackupOperation(ctx context.Context, podName, namespace, idempotencyKey string) (op instancemanager.Operation, found bool, err error)
}

// RestoreClient drives one instance's Instance Manager restore operation
// (ADR-0008). It is an interface so the reconciler can be tested without a live
// manager.
type RestoreClient interface {
	// StartRestore POSTs /v1/restore/prepare with the idempotency key and returns
	// the durable operation. A repeat with the same key returns the same operation.
	StartRestore(ctx context.Context, podName, namespace, idempotencyKey string, req instancemanager.RestoreRequest) (instancemanager.Operation, error)
	// GetOperation polls /v1/operations/{id}.
	GetOperation(ctx context.Context, podName, namespace, id string) (instancemanager.Operation, error)
	OperationFinder
}

// OperationFinder asks GET /v1/operations?idempotencyKey= for the operation
// of kind that the member holds under the key. It never starts anything.
// found is false only when the member answered that it holds none; any other
// answer is an error.
type OperationFinder interface {
	FindOperation(ctx context.Context, podName, namespace string, kind instancemanager.OperationKind,
		idempotencyKey string) (op instancemanager.Operation, found bool, err error)
}

// reasonTokenRefused is the condition and Event reason for a member whose
// Instance Manager refused the cluster's token.
const reasonTokenRefused = "InstanceManagerTokenRefused"

// errTokenRefused is wrapped by a call that the member answered 401 or 403.
// It never carries the token.
var errTokenRefused = errors.New("the member refused the cluster's Instance Manager token")

// errObjectStorageNotApplied is wrapped by a backup start that the member
// answered 412: its manager was started without object storage, and it runs
// no backup until the Pod is restarted with the cluster's settings.
var errObjectStorageNotApplied = errors.New("the member's Instance Manager runs without object storage")

// errStartConflict is wrapped by a backup start that the member answered
// 409: it holds an operation under the key for another request body,
// another operation runs on the database, or a running operation of another
// request owns the staging destination.
var errStartConflict = errors.New("the member refused the start as a conflict")

// errOperationInProgress is wrapped, with errStartConflict, by a start that
// the member refused because another operation that has not ended holds the
// database. It is retryable: the start is sent again once that one ended.
var errOperationInProgress = errors.New("another operation runs on the database")

// errStagingInUse is wrapped, with errStartConflict, by a backup start whose
// staging destination a running operation of another request owns. It is
// retryable like errOperationInProgress.
var errStagingInUse = errors.New("a running operation of another request owns the staging destination")

// errRequestConflict is wrapped, with errStartConflict, by a start that the
// member refused because it holds an operation under the same key for
// another request body. Sending the start again never succeeds.
var errRequestConflict = errors.New("the member holds an operation under this key for another request")

// errOperationRecordUnreadable is wrapped by a start that the member refused
// because its operation store holds a record it cannot read (500,
// OperationRecordUnreadable). Nothing was started; it is retryable once the
// record is readable again or removed by hand, so it uses up no attempt.
var errOperationRecordUnreadable = errors.New("the member's Instance Manager holds an operation record it cannot read")

// reasonOperationRecordUnreadable is the condition reason of a start refused
// with errOperationRecordUnreadable, and runbookUnreadableRecord the runbook
// its message points at.
const (
	reasonOperationRecordUnreadable = "InstanceManagerOperationRecordUnreadable"
	runbookUnreadableRecord         = "Runbook: an operation record that cannot be read"
)

// maxErrorBody bounds what is read of an error answer, and maxErrorMessage
// the length of the message taken from it. Of a larger answer at most
// maxErrorDrain bytes more are read and discarded.
const (
	maxErrorBody    = 4 << 10
	maxErrorMessage = 256
	maxErrorDrain   = 64 << 10
)

// errNotAnHAMember is wrapped, with errStartConflict, by an HA start that a
// standalone member refused. Nothing is held under the key; the member is
// not asked again before its Pod runs as an HA member.
var errNotAnHAMember = errors.New("the member is a standalone server, not an HA member")

// errRequestRefused is wrapped by a start that the member refused as a
// request it does not carry out (400, 413, 422): the same request is refused
// again, so it is a failed attempt, not a wait.
var errRequestRefused = errors.New("the member refused the request")

// Condition reasons of a start the member refused (4xx).
const (
	reasonRequestRefused      = "InstanceManagerRequestRefused"
	reasonNotAnHAMember       = "InstanceManagerNotAnHAMember"
	reasonOperationInProgress = "InstanceManagerOperationInProgress"
	reasonStagingInUse        = "InstanceManagerStagingInUse"
	reasonRequestConflict     = "InstanceManagerRequestConflict"
	reasonConflict            = "InstanceManagerConflict"
)

// tokenRefused reports whether an Instance Manager answer refuses the
// operator's token (401) or the call made with it (403). It only names the
// cause of a failed call: a 403 is not retried with the previous token and
// counts as accepted in acceptedTokens.send and AcceptsToken, which look at
// 401 alone. The Instance Manager does not answer 403 on any path.
func tokenRefused(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden
}

// unexpectedStatus describes an answer the call did not expect, with the
// cause and the message the member sent (readErrorAnswer). The cause comes
// from the answer's code; a manager that sends no code, or one this operator
// does not know, is judged by the status, as before codes existed (ADR-0003):
// 401 and 403 refuse the token, 412 means no object storage, and 409 is a
// conflict of no named cause.
func unexpectedStatus(resp *http.Response) error {
	code, msg := readErrorAnswer(resp)
	detail := fmt.Sprintf("status %d", resp.StatusCode)
	if code != "" {
		detail += ", " + code
	}
	if msg != "" {
		detail += ": " + msg
	}
	detail += requestRef(resp)
	switch {
	case tokenRefused(resp.StatusCode):
		return fmt.Errorf("%w (%s)", errTokenRefused, detail)
	case resp.StatusCode == http.StatusPreconditionFailed:
		return fmt.Errorf("%w (%s)", errObjectStorageNotApplied, detail)
	case code == instancemanager.CodeOperationRecordUnreadable:
		return fmt.Errorf("%w (%s); see the runbook %q in docs/observability.md",
			errOperationRecordUnreadable, detail, runbookUnreadableRecord)
	case resp.StatusCode != http.StatusConflict:
		return fmt.Errorf("unexpected %s", detail)
	}
	switch code {
	case instancemanager.CodeOperationInProgress:
		return fmt.Errorf("%w: %w (%s)", errStartConflict, errOperationInProgress, detail)
	case instancemanager.CodeStagingDestinationInUse:
		return fmt.Errorf("%w: %w (%s)", errStartConflict, errStagingInUse, detail)
	case instancemanager.CodeIdempotencyConflict:
		return fmt.Errorf("%w: %w (%s)", errStartConflict, errRequestConflict, detail)
	case instancemanager.CodeNotAnHAMember:
		return fmt.Errorf("%w: %w (%s)", errStartConflict, errNotAnHAMember, detail)
	}
	return fmt.Errorf("%w (%s)", errStartConflict, detail)
}

// startError describes an answer to a start that the call did not expect,
// like unexpectedStatus. Only an answer that refuses the request itself, as
// invalid (400), too large (413) or not processable (422), is refused again
// for the same request and wraps errRequestRefused. Any other 4xx means that
// the member could not be asked, for example an earlier Instance Manager
// without the route (404 or 405 from its mux), a timeout (408) or a member
// that sheds load (429), and is waited on like an unavailable member.
func startError(resp *http.Response) error {
	err := unexpectedStatus(resp)
	switch resp.StatusCode {
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		return fmt.Errorf("%w: %w", errRequestRefused, err)
	}
	return err
}

// readErrorAnswer reads the code and the message of an error answer
// (instancemanager.ErrorResponse). At most maxErrorBody bytes are read; a body
// that is larger, or no such answer, yields neither. The message ends up in
// conditions, Events and logs, so runs of characters that are not printable
// or are white space become one space, and it is cut to maxErrorMessage
// runes. A code that is not a plain word is dropped.
func readErrorAnswer(resp *http.Response) (code, msg string) {
	if resp.Body == nil {
		return "", ""
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody+1))
	if err != nil || len(raw) > maxErrorBody {
		// What is left is discarded, up to a bound, so that the connection
		// can be used again; a larger rest is left to Close.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorDrain))
		return "", ""
	}
	var answer instancemanager.ErrorResponse
	if json.Unmarshal(raw, &answer) != nil {
		return "", ""
	}
	if len(answer.Code) <= 64 && strings.IndexFunc(answer.Code, func(r rune) bool {
		return (r < 'A' || r > 'Z') && (r < 'a' || r > 'z')
	}) < 0 {
		code = answer.Code
	}
	return code, cleanMessage(answer.Error)
}

// cleanMessage makes a message from a member fit for conditions, Events and
// logs: runs of characters that are not printable or are white space become
// one space, and it is cut to maxErrorMessage runes.
func cleanMessage(msg string) string {
	cleaned := []rune(strings.Join(strings.FieldsFunc(msg, func(r rune) bool {
		return !unicode.IsPrint(r) || unicode.IsSpace(r)
	}), " "))
	if len(cleaned) > maxErrorMessage {
		cleaned = append(cleaned[:maxErrorMessage], []rune("...")...)
	}
	return string(cleaned)
}

// instanceManagerFailureReason is the condition reason for a failed call to
// an Instance Manager: a refused token, a manager without object storage,
// each cause of a conflict and an unreadable operation record are told apart
// from an unavailable manager.
func instanceManagerFailureReason(err error) string {
	switch {
	case errors.Is(err, errTokenRefused):
		return reasonTokenRefused
	case errors.Is(err, errObjectStorageNotApplied):
		return reasonObjectStorageNotApplied
	case errors.Is(err, errOperationInProgress):
		return reasonOperationInProgress
	case errors.Is(err, errStagingInUse):
		return reasonStagingInUse
	case errors.Is(err, errRequestConflict):
		return reasonRequestConflict
	case errors.Is(err, errNotAnHAMember):
		return reasonNotAnHAMember
	case errors.Is(err, errRequestRefused):
		return reasonRequestRefused
	case errors.Is(err, errStartConflict):
		return reasonConflict
	case errors.Is(err, errOperationRecordUnreadable):
		return reasonOperationRecordUnreadable
	}
	return reasonInstanceManagerUnavailable
}

// reasonInstanceManagerUnavailable is the reason of a failed call to an
// Instance Manager that is not told apart otherwise.
const reasonInstanceManagerUnavailable = "InstanceManagerUnavailable"

// operationOfKey answers a start of kind under key that the member refused as
// a conflict. The idempotency key names one request; a refusal because the
// member holds an operation under the key for another request body (the
// spec changed after the request was first sent, or an earlier operator
// built it differently) is answered by that operation, which find looks up,
// so it is followed to its end and the changed request is never started in
// its place. A refusal for another cause, or from a manager that names none,
// is looked up the same way unless it says that the database or the staging
// destination is busy, or that the member is no HA member, which holds
// nothing under the key and is retried. An
// operation that cannot be found leaves the refusal, with what the lookup
// said.
func operationOfKey(ctx context.Context, err error, kind instancemanager.OperationKind, key string,
	find func() (instancemanager.Operation, bool, error)) (instancemanager.Operation, error) {
	if !errors.Is(err, errStartConflict) || errors.Is(err, errOperationInProgress) || errors.Is(err, errStagingInUse) ||
		errors.Is(err, errNotAnHAMember) {
		return instancemanager.Operation{}, err
	}
	op, found, lookupErr := find()
	switch {
	case lookupErr != nil:
		// A lookup refused for the token is reported as the refusal.
		return instancemanager.Operation{}, fmt.Errorf("%w; the %s operation under the key %q could not be looked up: %w",
			err, kind, key, lookupErr)
	case !found:
		return instancemanager.Operation{}, fmt.Errorf("%w; the member holds no %s operation under the key %q", err, kind, key)
	}
	logf.FromContext(ctx).Info("Following the operation of the idempotency key instead of the changed request",
		"kind", kind, "idempotencyKey", key, "operationID", op.ID, "state", op.State)
	return op, nil
}

// TokenSource returns the bearer tokens a member's Instance Manager may hold,
// the current one first. During a token rotation the second is the previous
// token, which Pods started before the rotation hold.
type TokenSource func(ctx context.Context, podName, namespace string) ([]string, error)

// ClusterTokens reads a member's tokens from its own cluster's
// <cluster>-im-token Secret in the member's namespace, so that each cluster is
// called with its own credentials and no other. A member is a StatefulSet Pod
// named <cluster>-<ordinal>. A Secret without a current token yields none.
func ClusterTokens(c client.Reader) TokenSource {
	return func(ctx context.Context, podName, namespace string) ([]string, error) {
		i := strings.LastIndexByte(podName, '-')
		if i <= 0 {
			return nil, fmt.Errorf("%s is not the name of a cluster member", podName)
		}
		if _, err := strconv.ParseUint(podName[i+1:], 10, 32); err != nil {
			return nil, fmt.Errorf("%s is not the name of a cluster member", podName)
		}
		secret := &corev1.Secret{}
		key := types.NamespacedName{Name: imTokenSecretName(podName[:i]), Namespace: namespace}
		if err := c.Get(ctx, key, secret); err != nil {
			return nil, fmt.Errorf("read the Instance Manager token of %s: %w", podName, err)
		}
		current := string(secret.Data[imTokenKey])
		if current == "" {
			return nil, nil
		}
		tokens := []string{current}
		if previous := string(secret.Data[imPreviousTokenKey]); previous != "" && previous != current {
			tokens = append(tokens, previous)
		}
		return tokens, nil
	}
}

// HTTPBackupClient talks to the per-member Instance Manager over the pod's
// stable DNS (ADR-0004), port 9090 (ADR-0003), with the member's cluster token.
type HTTPBackupClient struct {
	Client *http.Client
	Tokens TokenSource

	accepted acceptedTokens
}

func NewHTTPBackupClient(tokens TokenSource) *HTTPBackupClient {
	return &HTTPBackupClient{
		Client: &http.Client{Timeout: 30 * time.Second},
		Tokens: tokens,
	}
}

func (c *HTTPBackupClient) StartBackup(ctx context.Context, podName, namespace, idempotencyKey string, req instancemanager.BackupRequest) (instancemanager.Operation, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return instancemanager.Operation{}, err
	}
	url := c.baseURL(podName, namespace) + "/v1/backup"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return instancemanager.Operation{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Idempotency-Key", idempotencyKey)
	resp, err := c.do(ctx, httpReq, podName, namespace)
	if err != nil {
		return instancemanager.Operation{}, fmt.Errorf("start backup on %s: %w", podName, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		return instancemanager.Operation{}, fmt.Errorf("start backup on %s: %w", podName, startError(resp))
	}
	return decodeOperation(resp)
}

func (c *HTTPBackupClient) StartRestore(ctx context.Context, podName, namespace, idempotencyKey string, req instancemanager.RestoreRequest) (instancemanager.Operation, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return instancemanager.Operation{}, err
	}
	url := c.baseURL(podName, namespace) + "/v1/restore/prepare"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return instancemanager.Operation{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Idempotency-Key", idempotencyKey)
	resp, err := c.do(ctx, httpReq, podName, namespace)
	if err != nil {
		return instancemanager.Operation{}, fmt.Errorf("start restore on %s: %w", podName, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		return instancemanager.Operation{}, fmt.Errorf("start restore on %s: %w", podName, startError(resp))
	}
	return decodeOperation(resp)
}

// StartHABootstrap asks one member to create the cluster's first database and
// start HA (ADR-0010).
func (c *HTTPBackupClient) StartHABootstrap(ctx context.Context, podName, namespace, idempotencyKey string, req instancemanager.HABootstrapRequest) (instancemanager.Operation, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return instancemanager.Operation{}, err
	}
	url := c.baseURL(podName, namespace) + "/v1/ha/bootstrap"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return instancemanager.Operation{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Idempotency-Key", idempotencyKey)
	resp, err := c.do(ctx, httpReq, podName, namespace)
	if err != nil {
		return instancemanager.Operation{}, fmt.Errorf("start HA bootstrap on %s: %w", podName, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		return instancemanager.Operation{}, fmt.Errorf("start HA bootstrap on %s: %w", podName, startError(resp))
	}
	return decodeOperation(resp)
}

func (c *HTTPBackupClient) GetOperation(ctx context.Context, podName, namespace, id string) (instancemanager.Operation, error) {
	url := fmt.Sprintf("%s/v1/operations/%s", c.baseURL(podName, namespace), id)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return instancemanager.Operation{}, err
	}
	resp, err := c.do(ctx, httpReq, podName, namespace)
	if err != nil {
		return instancemanager.Operation{}, fmt.Errorf("get operation %s on %s: %w", id, podName, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return instancemanager.Operation{}, fmt.Errorf("get operation %s on %s: %w", id, podName, unexpectedStatus(resp))
	}
	return decodeOperation(resp)
}

// FindBackupOperation reads the member's backup operation under
// idempotencyKey.
func (c *HTTPBackupClient) FindBackupOperation(ctx context.Context, podName, namespace, idempotencyKey string) (instancemanager.Operation, bool, error) {
	return c.FindOperation(ctx, podName, namespace, instancemanager.OpBackup, idempotencyKey)
}

// FindOperation reads the member's operation of kind under idempotencyKey. A
// manager that does not have the lookup answers 404, which is an error here:
// only an answered list says that the member holds no such operation.
func (c *HTTPBackupClient) FindOperation(ctx context.Context, podName, namespace string, kind instancemanager.OperationKind,
	idempotencyKey string) (instancemanager.Operation, bool, error) {
	target := c.baseURL(podName, namespace) + "/v1/operations?" + neturl.Values{"idempotencyKey": {idempotencyKey}}.Encode()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return instancemanager.Operation{}, false, err
	}
	resp, err := c.do(ctx, httpReq, podName, namespace)
	if err != nil {
		return instancemanager.Operation{}, false, fmt.Errorf("look up the %s operation on %s: %w", kind, podName, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return instancemanager.Operation{}, false, fmt.Errorf("look up the %s operation on %s: %w", kind, podName, unexpectedStatus(resp))
	}
	var list instancemanager.OperationList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil || list.Operations == nil {
		return instancemanager.Operation{}, false, fmt.Errorf("look up the %s operation on %s: unreadable answer%s", kind, podName, requestRef(resp))
	}
	for _, op := range list.Operations {
		if op.Kind == kind {
			return op, true, nil
		}
	}
	return instancemanager.Operation{}, false, nil
}

func (c *HTTPBackupClient) baseURL(podName, namespace string) string {
	return fmt.Sprintf("http://%s.%s.svc:%d", podName, namespace, instancemanager.DefaultPort)
}

// do sends the request to the member with its cluster's tokens.
func (c *HTTPBackupClient) do(ctx context.Context, req *http.Request, podName, namespace string) (*http.Response, error) {
	tokens, err := memberTokens(ctx, c.Tokens, podName, namespace)
	if err != nil {
		return nil, err
	}
	return c.accepted.send(c.Client, req, namespace+"/"+podName, tokens)
}

// memberTokens resolves the member's tokens. A member is never called without
// one: a missing or empty token is an error, not an unauthenticated call.
func memberTokens(ctx context.Context, tokens TokenSource, podName, namespace string) ([]string, error) {
	if tokens == nil {
		return nil, fmt.Errorf("no Instance Manager token source for %s", podName)
	}
	candidates, err := tokens(ctx, podName, namespace)
	if err != nil {
		return nil, err
	}
	if slices.Contains(candidates, "") {
		return nil, fmt.Errorf("no Instance Manager token for %s", podName)
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no Instance Manager token for %s", podName)
	}
	return candidates, nil
}

// acceptedTokens remembers, per member, a SHA-256 fingerprint of the token
// the member last accepted, never the token itself.
type acceptedTokens struct{ byMember sync.Map }

func tokenFingerprint(token string) [sha256.Size]byte { return sha256.Sum256([]byte(token)) }

// send sends the request with the first of the member's tokens and, while the
// member refuses with 401, again with the next one. An Instance Manager
// refuses a token it does not hold before it acts on the request, so a
// refused request has had no effect; only its authentication answers 401, so
// any other answer counts as accepted. The token the member accepted last
// time is tried first, so that a member that holds the previous token during
// a rotation is not sent a request it refuses on every call. The response of
// the last attempt is returned.
func (a *acceptedTokens) send(c *http.Client, req *http.Request, member string, tokens []string) (*http.Response, error) {
	if last, ok := a.byMember.Load(member); ok {
		i := slices.IndexFunc(tokens, func(t string) bool { return tokenFingerprint(t) == last.([sha256.Size]byte) })
		if i > 0 {
			tokens = append([]string{tokens[i]}, slices.Delete(slices.Clone(tokens), i, i+1)...)
		}
	}
	setRequestID(req)
	var resp *http.Response
	for i, token := range tokens {
		if resp != nil {
			_ = resp.Body.Close()
		}
		attempt := req
		if i > 0 {
			attempt = req.Clone(req.Context())
			if req.GetBody != nil {
				body, err := req.GetBody()
				if err != nil {
					return nil, err
				}
				attempt.Body = body
			}
		}
		attempt.Header.Set("Authorization", "Bearer "+token)
		var err error
		if resp, err = c.Do(attempt); err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusUnauthorized {
			a.byMember.Store(member, tokenFingerprint(token))
			break
		}
	}
	return resp, nil
}

// requestIDHeader names one call to an Instance Manager, which writes the
// same ID into its log (docs/observability.md).
const requestIDHeader = "X-Request-ID"

// setRequestID gives the call an ID of its own.
func setRequestID(req *http.Request) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return
	}
	req.Header.Set(requestIDHeader, hex.EncodeToString(b))
}

// requestRef names the call in an error, so that it can be found in the
// Instance Manager's log.
func requestRef(resp *http.Response) string {
	if id := resp.Header.Get(requestIDHeader); id != "" {
		return " (request " + id + ")"
	}
	return ""
}

func decodeOperation(resp *http.Response) (instancemanager.Operation, error) {
	var op instancemanager.Operation
	if err := json.NewDecoder(resp.Body).Decode(&op); err != nil {
		return instancemanager.Operation{}, fmt.Errorf("decode operation: %w", err)
	}
	return op, nil
}
