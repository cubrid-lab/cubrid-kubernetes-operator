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
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Prometheus queries a Prometheus server's HTTP API.
type Prometheus struct {
	URL  string
	HTTP *http.Client
}

type promResponse struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
	Data      struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string    `json:"metric"`
			Values [][2]json.RawMessage `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

// maxBody bounds what is read of an answer.
const maxBody = 8 << 20

// Range returns the raw samples of the series selector matches in the window
// that ends at at, each with the time Prometheus scraped it. A range query
// returns stored samples only, without Prometheus' staleness handling, so
// the caller judges how old the last sample is. A failed query is an error,
// never an empty result.
func (p *Prometheus) Range(ctx context.Context, selector string, window time.Duration, at time.Time) ([]Series, error) {
	q := url.Values{}
	q.Set("query", fmt.Sprintf("%s[%ds]", selector, int64(window/time.Second)))
	q.Set("time", strconv.FormatInt(at.Unix(), 10))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL+"/api/v1/query?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	httpClient := p.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, err
	}
	var r promResponse
	if err := json.Unmarshal(body, &r); err != nil {
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("prometheus answered %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("could not decode the answer of prometheus: %w", err)
	}
	if r.Status != "success" {
		return nil, fmt.Errorf("prometheus answered %d: %s: %s", resp.StatusCode, r.ErrorType, r.Error)
	}
	if r.Data.ResultType != "matrix" {
		return nil, fmt.Errorf("prometheus answered a %s, want a matrix", r.Data.ResultType)
	}
	out := make([]Series, 0, len(r.Data.Result))
	for _, res := range r.Data.Result {
		s := Series{Name: res.Metric["__name__"], Labels: map[string]string{}}
		for k, v := range res.Metric {
			if k != "__name__" {
				s.Labels[k] = v
			}
		}
		for _, pair := range res.Values {
			var ts float64
			var value string
			if err := json.Unmarshal(pair[0], &ts); err != nil {
				return nil, fmt.Errorf("sample time %s of %s: %w", pair[0], s.Name, err)
			}
			if err := json.Unmarshal(pair[1], &value); err != nil {
				return nil, fmt.Errorf("sample value %s of %s: %w", pair[1], s.Name, err)
			}
			v, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return nil, fmt.Errorf("sample value %q of %s: %w", value, s.Name, err)
			}
			s.Samples = append(s.Samples, Point{At: time.UnixMilli(int64(math.Round(ts * 1000))).UTC(), Value: v})
		}
		out = append(out, s)
	}
	return out, nil
}
