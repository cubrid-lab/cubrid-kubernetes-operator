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

package faults

import (
	"testing"
	"time"
)

func TestLimitFromEnv(t *testing.T) {
	const name = "E2E_TEST_LIMIT"
	tests := map[string]struct {
		value   string
		set     bool
		want    time.Duration
		wantErr bool
	}{
		"not set: the default":        {"", false, 30 * time.Second, false},
		"empty: the default":          {"", true, 30 * time.Second, false},
		"a duration":                  {"2m", true, 2 * time.Minute, false},
		"zero: the limit is not set":  {"0", true, 0, false},
		"unset: the limit is not set": {"unset", true, 0, false},
		"a number without a unit":     {"45", true, 0, true},
		"negative":                    {"-5s", true, 0, true},
		"not a duration":              {"soon", true, 0, true},
	}
	for label, tc := range tests {
		t.Run(label, func(t *testing.T) {
			if tc.set {
				t.Setenv(name, tc.value)
			}
			got, err := LimitFromEnv(name, 30*time.Second)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Errorf("got %s, %v; want %s, error %v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}
