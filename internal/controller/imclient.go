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
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

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
		return instancemanager.Operation{}, fmt.Errorf("start backup on %s: unexpected status %d%s", podName, resp.StatusCode, requestRef(resp))
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
		return instancemanager.Operation{}, fmt.Errorf("start restore on %s: unexpected status %d%s", podName, resp.StatusCode, requestRef(resp))
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
		return instancemanager.Operation{}, fmt.Errorf("start HA bootstrap on %s: unexpected status %d%s", podName, resp.StatusCode, requestRef(resp))
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
		return instancemanager.Operation{}, fmt.Errorf("get operation %s on %s: unexpected status %d%s", id, podName, resp.StatusCode, requestRef(resp))
	}
	return decodeOperation(resp)
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

// acceptedTokens remembers, per member, the token the member last accepted.
type acceptedTokens struct{ byMember sync.Map }

// send sends the request with the first of the member's tokens and, while the
// member refuses with 401, again with the next one. An Instance Manager
// refuses a token it does not hold before it acts on the request, so a
// refused request has had no effect. The token the member accepted last time
// is tried first, so that a member that holds the previous token during a
// rotation is not sent a request it refuses on every call. The response of
// the last attempt is returned.
func (a *acceptedTokens) send(c *http.Client, req *http.Request, member string, tokens []string) (*http.Response, error) {
	if last, ok := a.byMember.Load(member); ok {
		if i := slices.Index(tokens, last.(string)); i > 0 {
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
			a.byMember.Store(member, token)
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
