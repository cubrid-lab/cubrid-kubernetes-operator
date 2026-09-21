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
	"encoding/json"
	"fmt"
	"net/http"
	"time"

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

// HTTPBackupClient talks to the per-member Instance Manager over the pod's
// stable DNS (ADR-0004), port 9090 (ADR-0003), with the shared bearer token.
type HTTPBackupClient struct {
	Client *http.Client
	Token  string
}

func NewHTTPBackupClient(token string) *HTTPBackupClient {
	return &HTTPBackupClient{
		Client: &http.Client{Timeout: 30 * time.Second},
		Token:  token,
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
	c.authorize(httpReq)

	resp, err := c.Client.Do(httpReq)
	if err != nil {
		return instancemanager.Operation{}, fmt.Errorf("start backup on %s: %w", podName, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		return instancemanager.Operation{}, fmt.Errorf("start backup on %s: unexpected status %d", podName, resp.StatusCode)
	}
	return decodeOperation(resp)
}

func (c *HTTPBackupClient) GetOperation(ctx context.Context, podName, namespace, id string) (instancemanager.Operation, error) {
	url := fmt.Sprintf("%s/v1/operations/%s", c.baseURL(podName, namespace), id)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return instancemanager.Operation{}, err
	}
	c.authorize(httpReq)

	resp, err := c.Client.Do(httpReq)
	if err != nil {
		return instancemanager.Operation{}, fmt.Errorf("get operation %s on %s: %w", id, podName, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return instancemanager.Operation{}, fmt.Errorf("get operation %s on %s: unexpected status %d", id, podName, resp.StatusCode)
	}
	return decodeOperation(resp)
}

func (c *HTTPBackupClient) baseURL(podName, namespace string) string {
	return fmt.Sprintf("http://%s.%s.svc:%d", podName, namespace, instancemanager.DefaultPort)
}

func (c *HTTPBackupClient) authorize(req *http.Request) {
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
}

func decodeOperation(resp *http.Response) (instancemanager.Operation, error) {
	var op instancemanager.Operation
	if err := json.NewDecoder(resp.Body).Decode(&op); err != nil {
		return instancemanager.Operation{}, fmt.Errorf("decode operation: %w", err)
	}
	return op, nil
}
