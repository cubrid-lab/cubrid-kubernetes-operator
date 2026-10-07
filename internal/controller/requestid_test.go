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
	"io"
	"net/http"
	"strings"
	"testing"
)

// answer is a transport that records the request ID of each call and answers
// with a fixed status, returning the ID as an Instance Manager does.
type answer struct {
	status int
	ids    []string
}

func (a *answer) RoundTrip(req *http.Request) (*http.Response, error) {
	id := req.Header.Get(requestIDHeader)
	a.ids = append(a.ids, id)
	header := http.Header{}
	header.Set(requestIDHeader, id)
	return &http.Response{StatusCode: a.status, Header: header, Body: io.NopCloser(strings.NewReader("{}")), Request: req}, nil
}

// Every call to an Instance Manager carries an ID of its own, and a call
// that is answered with an error names that ID.
func TestInstanceManagerCalls_CarryARequestID(t *testing.T) {
	transport := &answer{status: http.StatusOK}
	prober := NewHTTPRoleProber(staticToken("tok"))
	prober.Client = &http.Client{Transport: transport}
	prober.ProbeRole(context.Background(), "demo-0", "ns")
	prober.ProbeRole(context.Background(), "demo-0", "ns")
	if len(transport.ids) != 2 || transport.ids[0] == "" || transport.ids[0] == transport.ids[1] {
		t.Errorf("request IDs of two role probes = %q", transport.ids)
	}

	failing := &answer{status: http.StatusInternalServerError}
	client := NewHTTPBackupClient(staticToken("tok"))
	client.Client = &http.Client{Transport: failing}
	_, err := client.GetOperation(context.Background(), "demo-0", "ns", "op-1")
	if err == nil || len(failing.ids) != 1 || failing.ids[0] == "" || !strings.Contains(err.Error(), failing.ids[0]) {
		t.Errorf("error = %v, request IDs = %q; want the error to name the request", err, failing.ids)
	}
}
