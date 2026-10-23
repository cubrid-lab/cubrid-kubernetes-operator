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

package observer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func prometheusAnswering(t *testing.T, status int, body string, check func(*http.Request)) *Prometheus {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if check != nil {
			check(r)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &Prometheus{URL: srv.URL, HTTP: srv.Client()}
}

func TestPrometheusRangeReturnsEveryRawSampleWithItsScrapeTime(t *testing.T) {
	body := `{"status":"success","data":{"resultType":"matrix","result":[
	  {"metric":{"__name__":"cubrid_cluster_ready","namespace":"ns","cluster":"ha"},
	   "values":[[1792756174.281,"1"],[1792756204.281,"0"]]},
	  {"metric":{"__name__":"cubrid_instance_role","namespace":"ns","cluster":"ha","member":"ha-0","role":"master"},
	   "values":[[1792756204.281,"1"]]}]}}`
	evaluated := time.Unix(1792756210, 0)
	p := prometheusAnswering(t, http.StatusOK, body, func(r *http.Request) {
		if r.URL.Path != "/api/v1/query" {
			t.Errorf("path = %q, want /api/v1/query", r.URL.Path)
		}
		if got, want := r.URL.Query().Get("query"), `{cluster="ha"}[90s]`; got != want {
			t.Errorf("query = %q, want %q", got, want)
		}
		if got, want := r.URL.Query().Get("time"), "1792756210"; got != want {
			t.Errorf("time = %q, want %q", got, want)
		}
	})
	got, err := p.Range(context.Background(), `{cluster="ha"}`, 90*time.Second, evaluated)
	if err != nil {
		t.Fatalf("Range() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Range() = %+v, want two series", got)
	}
	ready := got[0]
	if ready.Name != "cubrid_cluster_ready" || ready.Labels["__name__"] != "" || ready.Labels["cluster"] != "ha" {
		t.Fatalf("series = %+v, want the name taken out of the labels", ready)
	}
	if len(ready.Samples) != 2 || ready.Samples[1].Value != 0 ||
		!ready.Samples[1].At.Equal(time.UnixMilli(1792756204281)) {
		t.Fatalf("samples = %+v, want both raw samples with their millisecond scrape times", ready.Samples)
	}
}

func TestPrometheusRangeReportsAFailedQuery(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
		want   string
	}{
		"server error":     {http.StatusServiceUnavailable, `upstream down`, "503"},
		"query error":      {http.StatusBadRequest, `{"status":"error","errorType":"bad_data","error":"parse error"}`, "parse error"},
		"not a matrix":     {http.StatusOK, `{"status":"success","data":{"resultType":"vector","result":[]}}`, "vector"},
		"value not number": {http.StatusOK, `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[[1,"x"]]}]}}`, "x"},
		"not json":         {http.StatusOK, `<html>`, "decode"},
	} {
		t.Run(name, func(t *testing.T) {
			p := prometheusAnswering(t, tc.status, tc.body, nil)
			got, err := p.Range(context.Background(), `up`, time.Minute, time.Unix(100, 0))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Range() = %+v, %v; want an error containing %q", got, err, tc.want)
			}
		})
	}
}

func TestPrometheusRangeReturnsNoSeriesForAnEmptyResult(t *testing.T) {
	p := prometheusAnswering(t, http.StatusOK, `{"status":"success","data":{"resultType":"matrix","result":[]}}`, nil)
	got, err := p.Range(context.Background(), `up`, time.Minute, time.Unix(100, 0))
	if err != nil || len(got) != 0 {
		t.Fatalf("Range() = %+v, %v; want no series and no error", got, err)
	}
}
