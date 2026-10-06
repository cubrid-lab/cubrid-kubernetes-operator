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
	"fmt"
	"os"
	"time"
)

// LimitFromEnv returns the time limit a scenario is judged by: the value of
// the environment variable name when it is set, and def otherwise. The value
// is a Go duration such as "45s" or "2m"; "0" or "unset" means that the
// limit is not set, so that the scenario runs as a baseline and is reported
// as blocked. A value that is not a duration, or is negative, is an error:
// a mistyped limit must not silently become the default.
func LimitFromEnv(name string, def time.Duration) (time.Duration, error) {
	value, set := os.LookupEnv(name)
	if !set || value == "" {
		return def, nil
	}
	if value == "unset" {
		return 0, nil
	}
	limit, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s=%q is not a duration such as 45s or 2m: %w", name, value, err)
	}
	if limit < 0 {
		return 0, fmt.Errorf("%s=%q is negative", name, value)
	}
	return limit, nil
}
