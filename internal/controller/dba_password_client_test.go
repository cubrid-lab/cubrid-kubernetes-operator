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

	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
)

// dbaPasswordAnswer is a transport that records each call and answers it
// with status and body.
type dbaPasswordAnswer struct {
	status int
	body   string
	calls  []*http.Request
}

func (a *dbaPasswordAnswer) RoundTrip(req *http.Request) (*http.Response, error) {
	a.calls = append(a.calls, req)
	return &http.Response{StatusCode: a.status, Header: http.Header{},
		Body: io.NopCloser(strings.NewReader(a.body)), Request: req}, nil
}

// The operator asks the member's Instance Manager with the cluster's token,
// and takes only one of the three answers it knows; anything else is an
// error, which the reconciler treats as a check that could not tell.
func TestHTTPBackupClient_CheckDBAPassword(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		want    instancemanager.DBAPasswordState
		wantErr string
	}{
		{name: "a value that matches", status: http.StatusOK, body: `{"state":"Matches"}`, want: instancemanager.DBAPasswordMatches},
		{name: "a value that differs", status: http.StatusOK, body: `{"state":"Differs","reason":"refused"}`,
			want: instancemanager.DBAPasswordDiffers},
		{name: "a check that could not tell", status: http.StatusOK, body: `{"state":"Unknown","reason":"not running"}`,
			want: instancemanager.DBAPasswordUnknown},
		{name: "an answer it does not know", status: http.StatusOK, body: `{"state":"Maybe"}`, wantErr: "Maybe"},
		{name: "an answer it cannot read", status: http.StatusOK, body: `not json`, wantErr: "unreadable"},
		{name: "an Instance Manager without the route", status: http.StatusNotFound, body: `404 page not found`,
			wantErr: "status 404"},
		{name: "a token the member refuses", status: http.StatusUnauthorized, body: `{}`, wantErr: "refused the cluster's"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transport := &dbaPasswordAnswer{status: tc.status, body: tc.body}
			c := NewHTTPBackupClient(staticToken("tok"))
			c.Client = &http.Client{Transport: transport}
			got, err := c.CheckDBAPassword(context.Background(), "demo-0", "ns")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("CheckDBAPassword = %+v, %v; want an error saying %q", got, err, tc.wantErr)
				}
			} else if err != nil || got.State != tc.want {
				t.Fatalf("CheckDBAPassword = %+v, %v; want %q", got, err, tc.want)
			}
			if len(transport.calls) == 0 {
				t.Fatal("no call was sent")
			}
			req := transport.calls[0]
			if req.Method != http.MethodGet || req.URL.String() != "http://demo-0.ns.svc:9090/v1/dba-password" {
				t.Errorf("call = %s %s, want GET of the member's /v1/dba-password", req.Method, req.URL)
			}
			if req.Header.Get("Authorization") != "Bearer tok" {
				t.Errorf("the call was not sent with the cluster's token")
			}
		})
	}
}
