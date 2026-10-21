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
	cmd.Env = append(os.Environ(), "FETCH_BACKOFF=0")
	var errOut strings.Builder
	cmd.Stderr = &errOut
	err = cmd.Run()
	got, _ = os.ReadFile(out)
	if _, statErr := os.Stat(out + ".part"); !os.IsNotExist(statErr) {
		t.Errorf("the unverified download %s.part was left behind (stat: %v)", out, statErr)
	}
	return errOut.String(), got, err
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
