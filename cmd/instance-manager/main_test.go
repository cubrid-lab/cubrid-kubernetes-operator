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

package main

import (
	"io"
	"log/slog"
	"strings"
	"testing"
)

// A manager without a token does not start: it would serve its management
// API to nobody (#271).
func TestValidateToken(t *testing.T) {
	for _, token := range []string{"", " ", " \n\t"} {
		if err := validateToken(token); err == nil || !strings.Contains(err.Error(), "IM_TOKEN") {
			t.Errorf("validateToken(%q) = %v, want a refusal that names IM_TOKEN", token, err)
		}
	}
	if err := validateToken("0123abcd"); err != nil {
		t.Errorf("validateToken of a token = %v, want nil", err)
	}
}

// run checks the token before it listens or opens anything.
func TestRun_RefusesEmptyToken(t *testing.T) {
	t.Setenv("IM_TOKEN", "")
	t.Setenv("IM_ADDR", "127.0.0.1:0")
	err := run(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil || !strings.Contains(err.Error(), "IM_TOKEN") {
		t.Fatalf("run = %v, want a refusal that names IM_TOKEN", err)
	}
}
