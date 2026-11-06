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

package workload

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// fetchVerified runs test/workload/client/fetch-verified.sh against a server
// whose answer to the n-th request (from 1) is serve(n), and returns the
// script's stderr, the downloaded file's content, if any, and its error.
func fetchVerified(t *testing.T, want []byte, attempts string,
	serve func(n int32, w http.ResponseWriter)) (stderr string, got []byte, err error) {
	t.Helper()
	stderr, got, _, err = fetchVerifiedWaits(t, want, attempts, nil, serve)
	return stderr, got, err
}

// fetchVerifiedWaits is fetchVerified with extra environment settings. A
// stand-in sleep command records each requested wait instead of waiting, so
// the test can check the waits without taking the time, and it returns them
// in seconds, in order.
func fetchVerifiedWaits(t *testing.T, want []byte, attempts string, env []string,
	serve func(n int32, w http.ResponseWriter)) (stderr string, got []byte, waits []string, err error) {
	t.Helper()
	for _, tool := range []string{"curl", "sha256sum"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " is not installed")
		}
	}
	// Reading the script makes it an input of the test for Go's test cache.
	script := filepath.Join("client", "fetch-verified.sh")
	if _, err := os.ReadFile(script); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		serve(requests.Add(1), w)
	}))
	defer srv.Close()
	sum := sha256.Sum256(want)
	out := filepath.Join(t.TempDir(), "driver.jar")
	cmd := exec.Command("sh", script, srv.URL+"/driver.jar",
		hex.EncodeToString(sum[:]), out, attempts)
	binDir := t.TempDir()
	waitsFile := filepath.Join(binDir, "waits")
	fakeSleep := "#!/bin/sh\necho \"$1\" >> '" + waitsFile + "'\n"
	if err := os.WriteFile(filepath.Join(binDir, "sleep"), []byte(fakeSleep), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd.Env = append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"), "FETCH_BACKOFF=0")
	cmd.Env = append(cmd.Env, env...)
	var errOut strings.Builder
	cmd.Stderr = &errOut
	err = cmd.Run()
	got, _ = os.ReadFile(out)
	for _, leftover := range []string{out + ".part", out + ".headers"} {
		if _, statErr := os.Stat(leftover); !os.IsNotExist(statErr) {
			t.Errorf("the temporary file %s was left behind (stat: %v)", leftover, statErr)
		}
	}
	recorded, _ := os.ReadFile(waitsFile)
	return errOut.String(), got, strings.Fields(string(recorded)), err
}

// One bad answer, a body with another digest or a server error, is asked for
// again, and the verified body is kept (#521).
func TestFetchVerified_RetriesABadAnswer(t *testing.T) {
	good := []byte("the driver")
	for name, first := range map[string]func(w http.ResponseWriter){
		"another body":   func(w http.ResponseWriter) { _, _ = w.Write([]byte("<html>try again later</html>")) },
		"a server error": func(w http.ResponseWriter) { w.WriteHeader(http.StatusServiceUnavailable) },
	} {
		t.Run(name, func(t *testing.T) {
			stderr, got, err := fetchVerified(t, good, "3", func(n int32, w http.ResponseWriter) {
				if n == 1 {
					first(w)
					return
				}
				_, _ = w.Write(good)
			})
			if err != nil || string(got) != string(good) {
				t.Errorf("err = %v, file = %q, want the verified body; stderr:\n%s", err, got, stderr)
			}
			if !strings.Contains(stderr, "attempt 1 of 3") {
				t.Errorf("the discarded attempt is not reported: %s", stderr)
			}
		})
	}
}

// A body that keeps its other digest fails after the attempts, names what it
// received and what was wanted, and leaves no file behind.
func TestFetchVerified_RefusesABodyThatStaysWrong(t *testing.T) {
	good := []byte("the driver")
	bad := []byte("not the driver")
	stderr, got, err := fetchVerified(t, good, "2", func(_ int32, w http.ResponseWriter) { _, _ = w.Write(bad) })
	if err == nil || got != nil {
		t.Fatalf("err = %v, file = %q; want a failure and no file", err, got)
	}
	badSum := sha256.Sum256(bad)
	goodSum := sha256.Sum256(good)
	for _, want := range []string{hex.EncodeToString(badSum[:]), hex.EncodeToString(goodSum[:]), "after 2 attempts"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr does not name %q:\n%s", want, stderr)
		}
	}
}

// backoffOne makes the rate-limit backoff 15 s for the first wait.
var backoffOne = []string{"FETCH_BACKOFF=1"}

func rateLimited(retryAfter string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.WriteHeader(http.StatusTooManyRequests)
	}
}

// A rate-limited answer (429) that names a wait in Retry-After is waited out
// for that long, and the verified body that follows is kept (#718).
func TestFetchVerified_WaitsForRetryAfter(t *testing.T) {
	good := []byte("the driver")
	stderr, got, waits, err := fetchVerifiedWaits(t, good, "3", nil, func(n int32, w http.ResponseWriter) {
		if n == 1 {
			rateLimited("7")(w)
			return
		}
		_, _ = w.Write(good)
	})
	if err != nil || string(got) != string(good) {
		t.Fatalf("err = %v, file = %q, want the verified body; stderr:\n%s", err, got, stderr)
	}
	if strings.Join(waits, ",") != "7" {
		t.Errorf("waits = %v, want [7] from Retry-After", waits)
	}
}

// Without Retry-After a rate-limited answer is waited out with a longer
// backoff than other failures, which keep the short wait of FETCH_BACKOFF.
func TestFetchVerified_RateLimitBacksOffLongerThanOtherFailures(t *testing.T) {
	good := []byte("the driver")
	serveAfter := func(first func(w http.ResponseWriter)) func(int32, http.ResponseWriter) {
		return func(n int32, w http.ResponseWriter) {
			if n <= 2 {
				first(w)
				return
			}
			_, _ = w.Write(good)
		}
	}
	env := backoffOne
	_, _, limited, err := fetchVerifiedWaits(t, good, "3", env, serveAfter(rateLimited("")))
	if err != nil {
		t.Fatal(err)
	}
	_, _, other, err := fetchVerifiedWaits(t, good, "3", env, serveAfter(func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(other, ",") != "1,2" {
		t.Errorf("waits after a plain failure = %v, want [1 2] as before", other)
	}
	if strings.Join(limited, ",") != "15,30" {
		t.Errorf("waits after 429 = %v, want [15 30]", limited)
	}
}

// A 503 is treated like a 429.
func TestFetchVerified_ServiceUnavailableIsWaitedOutWithRetryAfter(t *testing.T) {
	good := []byte("the driver")
	_, got, waits, err := fetchVerifiedWaits(t, good, "2", nil, func(n int32, w http.ResponseWriter) {
		if n == 1 {
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write(good)
	})
	if err != nil || string(got) != string(good) || strings.Join(waits, ",") != "3" {
		t.Errorf("err = %v, file = %q, waits = %v; want success after one wait of 3", err, got, waits)
	}
}

// A Retry-After the server sends never causes more than the cap for one
// wait, nor a non-numeric sleep: a huge value is cut to the cap, and text or
// an HTTP date falls back to the backoff.
func TestFetchVerified_RetryAfterIsBounded(t *testing.T) {
	good := []byte("the driver")
	for name, c := range map[string]struct{ header, want string }{
		"a huge number":          {"99999999999999999999999999", "20"},
		"a number above the cap": {"500", "20"},
		"leading zeros":          {"0017", "17"},
		"text":                   {"soon; rm -rf /", "15"},
		"an HTTP date":           {"Wed, 21 Oct 2026 07:28:00 GMT", "15"},
		"a negative number":      {"-5", "15"},
		"an empty value":         {" ", "15"},
		"a fraction":             {"1.5", "15"},
	} {
		t.Run(name, func(t *testing.T) {
			env := []string{backoffOne[0], "FETCH_RATE_LIMIT_CAP=20"}
			_, got, waits, err := fetchVerifiedWaits(t, good, "2", env, func(n int32, w http.ResponseWriter) {
				if n == 1 {
					rateLimited(c.header)(w)
					return
				}
				_, _ = w.Write(good)
			})
			if err != nil || string(got) != string(good) {
				t.Fatalf("err = %v, file = %q, want the verified body", err, got)
			}
			if strings.Join(waits, ",") != c.want {
				t.Errorf("waits = %v, want [%s]", waits, c.want)
			}
		})
	}
}

// A server that rate-limits forever fails, nothing is kept, and the waits
// together stay within the budget even when Retry-After asks for more.
func TestFetchVerified_RateLimitForeverFailsWithinTheBudget(t *testing.T) {
	good := []byte("the driver")
	env := []string{"FETCH_RATE_LIMIT_CAP=40", "FETCH_RATE_LIMIT_BUDGET=100"}
	stderr, got, waits, err := fetchVerifiedWaits(t, good, "6", env, func(_ int32, w http.ResponseWriter) {
		rateLimited("1000")(w)
	})
	if err == nil || got != nil {
		t.Fatalf("err = %v, file = %q; want a failure and no file", err, got)
	}
	total := 0
	for _, w := range waits {
		n, convErr := strconv.Atoi(w)
		if convErr != nil || n > 40 {
			t.Errorf("wait %q is not a number up to the cap of 40", w)
		}
		total += n
	}
	if total > 100 {
		t.Errorf("waited %d s in total (%v), want at most the budget of 100", total, waits)
	}
	if strings.Join(waits, ",") != "40,40,20" {
		t.Errorf("waits = %v, want [40 40 20] and no attempt after the budget is used up", waits)
	}
	if !strings.Contains(stderr, "rate limit") {
		t.Errorf("stderr does not say that the server rate-limited: %s", stderr)
	}
}

// A body with another digest is discarded even after a rate-limited wait.
func TestFetchVerified_WrongDigestAfterRateLimitIsStillDiscarded(t *testing.T) {
	good := []byte("the driver")
	stderr, got, _, err := fetchVerifiedWaits(t, good, "3", nil, func(n int32, w http.ResponseWriter) {
		if n == 1 {
			rateLimited("1")(w)
			return
		}
		_, _ = w.Write([]byte("not the driver"))
	})
	if err == nil || got != nil {
		t.Errorf("err = %v, file = %q; want a failure and no file; stderr:\n%s", err, got, stderr)
	}
}

// Retry-After is a minimum: a small value never shortens the wait below the
// rate-limit backoff, so Retry-After: 0 and 1 behave like no header, while a
// larger value is followed.
func TestFetchVerified_RetryAfterIsAMinimum(t *testing.T) {
	good := []byte("the driver")
	for header, want := range map[string]string{"0": "15", "1": "15", "14": "15", "100": "100"} {
		t.Run("Retry-After "+header, func(t *testing.T) {
			_, got, waits, err := fetchVerifiedWaits(t, good, "2", backoffOne, func(n int32, w http.ResponseWriter) {
				if n == 1 {
					rateLimited(header)(w)
					return
				}
				_, _ = w.Write(good)
			})
			if err != nil || string(got) != string(good) || strings.Join(waits, ",") != want {
				t.Errorf("err = %v, file = %q, waits = %v; want success after one wait of %s", err, got, waits, want)
			}
		})
	}
}

// Only the Retry-After of the final answer counts: one sent by a redirect on
// the way to a 429 is ignored.
func TestFetchVerified_IgnoresRetryAfterOfARedirect(t *testing.T) {
	good := []byte("the driver")
	_, got, waits, err := fetchVerifiedWaits(t, good, "2", backoffOne, func(n int32, w http.ResponseWriter) {
		switch n {
		case 1:
			w.Header().Set("Retry-After", "77")
			w.Header().Set("Location", "/elsewhere")
			w.WriteHeader(http.StatusFound)
		case 2:
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			_, _ = w.Write(good)
		}
	})
	if err != nil || string(got) != string(good) || strings.Join(waits, ",") != "15" {
		t.Errorf("err = %v, file = %q, waits = %v; want success after one wait of 15", err, got, waits)
	}
}

// The documented defaults: 5 attempts, waits of 30, 60, 90 and 120 s with
// FETCH_BACKOFF=2, and a failure after the last attempt.
func TestFetchVerified_DocumentedDefaultWaits(t *testing.T) {
	good := []byte("the driver")
	var requests int32
	stderr, got, waits, err := fetchVerifiedWaits(t, good, "", []string{"FETCH_BACKOFF=2"},
		func(n int32, w http.ResponseWriter) {
			requests = n
			rateLimited("")(w)
		})
	if err == nil || got != nil || requests != 5 || strings.Join(waits, ",") != "30,60,90,120" {
		t.Errorf("err = %v, requests = %d, waits = %v; want a failure after 5 attempts "+
			"and waits 30,60,90,120; stderr:\n%s", err, requests, waits, stderr)
	}
}

// The documented default cap for one wait is 120 s, and the default budget
// for all waits is 300 s: Retry-After: 120 is waited out twice, then 60 s of
// the budget remain, then the script gives up without another attempt.
func TestFetchVerified_DocumentedDefaultCapAndBudget(t *testing.T) {
	good := []byte("the driver")
	_, _, waits, err := fetchVerifiedWaits(t, good, "10", nil, func(_ int32, w http.ResponseWriter) {
		rateLimited("500")(w)
	})
	if err == nil || strings.Join(waits, ",") != "120,120,60" {
		t.Errorf("err = %v, waits = %v; want a failure after waits 120,120,60", err, waits)
	}
}
