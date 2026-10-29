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

package installed

import (
	"context"
	"encoding/base64"
	"errors"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

// managerLogKey and memberLogKey are the logs S11 searches: the installed
// manager's, and the Instance Manager's in a member Pod.
func managerLogKey(cfg HAConfig, previous bool) string {
	return logKey(cfg.OperatorNamespace, testManagerPodName, managerContainer, previous)
}

func memberLogKey(cfg HAConfig, pod string) string {
	return logKey(cfg.Namespace, pod, databaseContainer, false)
}

// requireLogsSearched checks that the installed manager's log and the
// Instance Manager's log in target were searched whole and found clean.
func requireLogsSearched(t *testing.T, creds *s11Credentials, target string) {
	t.Helper()
	if creds == nil || len(creds.Logs) != 2 {
		t.Fatalf("credentials = %+v, want two logs searched", creds)
	}
	op, im := creds.Logs[0], creds.Logs[1]
	if op.Log != operatorLog || op.Pod != testManagerPodName || op.UID != testManagerPodName ||
		op.Container != managerContainer || im.Log != instanceManagerLog || im.Pod != target || im.UID == "" ||
		im.Container != databaseContainer {
		t.Fatalf("logs = %+v, want the manager's and the Instance Manager's in %s", creds.Logs, target)
	}
	for _, c := range creds.Logs {
		if c.SinceTime.IsZero() || c.BytesRead == 0 || c.Truncated || c.PreviousRead || c.Found || c.Unchecked != "" {
			t.Fatalf("log = %+v, want it read whole and clean", c)
		}
	}
}

// restartedManager reads the installed manager's Pod with one restart of
// its container.
type restartedManager struct{ client.Client }

func (c restartedManager) Get(ctx context.Context, key client.ObjectKey, obj client.Object,
	opts ...client.GetOption) error {
	if err := c.Client.Get(ctx, key, obj, opts...); err != nil {
		return err
	}
	if pod, ok := obj.(*corev1.Pod); ok && pod.Name == testManagerPodName {
		for i := range pod.Status.ContainerStatuses {
			pod.Status.ContainerStatuses[i].RestartCount = 1
		}
	}
	return nil
}

// runS11Logs runs S11 complete with the fixture's logs and returns it and
// its record. The evidence holds no seed key, whatever the logs held.
func runS11Logs(t *testing.T, f *s11Fixture) (evidence.Scenario, s11Record) {
	t.Helper()
	_ = f.runner.RunHA(t.Context(), f.cfg)
	_, _, s11 := s11Scenario(t, f.cfg.EvidenceDir)
	requireNoSecretInEvidence(t, f.cfg.EvidenceDir)
	return s11, readS11Record(t, f.cfg.EvidenceDir)
}

func TestS11CompleteSearchesOnlyTheCurrentInstanceOfAContainerThatDidNotRestart(t *testing.T) {
	cfg := testS11Config(t)
	f := newS11Fixture(t, cfg)
	s11, rec := runS11Logs(t, f)
	if s11.Result != evidence.Pass {
		t.Fatalf("S11 = %+v, want a pass with clean logs", s11)
	}
	requireLogsSearched(t, rec.Credentials, testBackupTarget)
	if want := []string{managerLogKey(cfg, false), memberLogKey(cfg, testBackupTarget)}; !slices.Equal(
		f.logReads, want) {
		t.Fatalf("logs read = %v, want %v", f.logReads, want)
	}
}

func TestS11CompleteFailsOnAKeyInALog(t *testing.T) {
	for name, tc := range map[string]struct {
		log     func(HAConfig) string
		content string
		// at is the index of the log in the record.
		at int
	}{
		"seed access key in the Operator's log": {log: func(c HAConfig) string { return managerLogKey(c, false) },
			content: "level=info msg=\"Read object store\" access=" + testAccessKey},
		"seed secret key in the Instance Manager's log": {log: func(c HAConfig) string {
			return memberLogKey(c, testBackupTarget)
		}, content: "upload with secret " + testSecretKey, at: 1},
		"base64 secret key in the Instance Manager's log": {log: func(c HAConfig) string {
			return memberLogKey(c, testBackupTarget)
		}, content: "auth " + base64.StdEncoding.EncodeToString([]byte(testSecretKey)), at: 1},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS11Config(t)
			f := newS11Fixture(t, cfg)
			f.logs[tc.log(cfg)] = "first line\n" + tc.content + "\nlast line\n"
			s11, rec := runS11Logs(t, f)
			want := rec.Credentials.Logs[tc.at].Log
			if s11.Result != evidence.Fail || len(s11.NeverEvents) != 1 ||
				!strings.Contains(s11.NeverEvents[0], want) || strings.Contains(s11.Reason, testAccessKey) ||
				strings.Contains(s11.Reason, testSecretKey) {
				t.Fatalf("S11 = %+v, want a never-event naming %s and no key", s11, want)
			}
			if c := rec.Credentials.Logs[tc.at]; !c.Found || c.Unchecked != "" {
				t.Fatalf("log = %+v, want the key found", c)
			}
			if c := rec.Credentials.Logs[1-tc.at]; c.Found {
				t.Fatalf("log = %+v, want the other log clean", c)
			}
		})
	}
}

func TestS11CompleteFailsOnAKeyInThePreviousInstanceOfARestartedContainer(t *testing.T) {
	cfg := testS11Config(t)
	f := newS11Fixture(t, cfg)
	// The manager restarted once: its previous instance logged the key.
	f.runner.Client = restartedManager{Client: f.runner.Client}
	f.logs[managerLogKey(cfg, true)] = "panic: dump " + testSecretKey + "\n"
	s11, rec := runS11Logs(t, f)
	if s11.Result != evidence.Fail || len(s11.NeverEvents) != 1 || !strings.Contains(s11.NeverEvents[0], operatorLog) {
		t.Fatalf("S11 = %+v, want a never-event naming the Operator's log", s11)
	}
	if c := rec.Credentials.Logs[0]; !c.Found || !c.PreviousRead || c.RestartCount != 1 || c.PreviousBytesRead == 0 {
		t.Fatalf("log = %+v, want the previous instance read and the key found", c)
	}
	if !slices.Contains(f.logReads, managerLogKey(cfg, true)) {
		t.Fatalf("logs read = %v, want the previous instance", f.logReads)
	}
}

func TestS11CompleteIsBlockedByALogNotFullySearched(t *testing.T) {
	for name, tc := range map[string]struct {
		prepare func(*s11Fixture)
		want    string
		check   func(s11LogCheck) bool
	}{
		"unreadable Instance Manager's log": {prepare: func(f *s11Fixture) {
			f.logErr[memberLogKey(f.cfg, testBackupTarget)] = apierrors.NewForbidden(
				corev1.Resource("pods/log"), testBackupTarget, errors.New("RBAC"))
		}, want: testForbidden, check: func(c s11LogCheck) bool { return c.Log == instanceManagerLog }},
		"truncated Operator's log": {prepare: func(f *s11Fixture) {
			limit := maxLogBytes
			maxLogBytes = 64
			f.t.Cleanup(func() { maxLogBytes = limit })
			f.logs[managerLogKey(f.cfg, false)] = strings.Repeat("reconciled\n", 10)
		}, want: "more than 64 bytes", check: func(c s11LogCheck) bool {
			return c.Log == operatorLog && c.Truncated && c.BytesRead == 65
		}},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testS11Config(t)
			f := newS11Fixture(t, cfg)
			tc.prepare(f)
			s11, rec := runS11Logs(t, f)
			if s11.Result != evidence.Blocked || len(s11.NeverEvents) != 0 ||
				!strings.Contains(s11.Reason, "was not fully searched") || !strings.Contains(s11.Reason, tc.want) {
				t.Fatalf("S11 = %+v, want it blocked on %q", s11, tc.want)
			}
			if !slices.ContainsFunc(rec.Credentials.Logs, func(c s11LogCheck) bool {
				return tc.check(c) && c.Unchecked != "" && !c.Found
			}) {
				t.Fatalf("logs = %+v, want what was not searched recorded", rec.Credentials.Logs)
			}
		})
	}
}

func TestS11MissingDestinationFailsOnAKeyInTheInstanceManagersLog(t *testing.T) {
	cfg := testS11MissingConfig(t)
	f := newS11MissingFixture(t, cfg)
	// S11 complete reads the same log first; the key is logged after it.
	reads := 0
	f.logOf = func(key string) (string, bool) {
		if key != memberLogKey(cfg, testBackupTarget) {
			return "", false
		}
		if reads++; reads == 1 {
			return "clean\n", true
		}
		return "put with " + testAccessKey + "\n", true
	}
	missing, _ := missingOf(t, f)
	if missing.Result != evidence.Fail || !strings.Contains(missing.Reason, instanceManagerLog) {
		t.Fatalf("S11 missing-destination = %+v, want a failure naming the Instance Manager's log", missing)
	}
}

func TestS11DeniedCredentialsFailsOnADeniedKeyInTheOperatorsLog(t *testing.T) {
	cfg := testS11DeniedConfig(t)
	f := newS11DeniedFixture(t, cfg)
	// The denied keys exist once the variant made them, after S11 complete.
	f.logOf = func(key string) (string, bool) {
		if key != managerLogKey(cfg, false) || len(f.keys) == 0 {
			return "", false
		}
		return "Secret object-storage-denied holds " + f.keys[1] + "\n", true
	}
	denied, _ := deniedOf(t, f)
	if denied.Result != evidence.Fail || !strings.Contains(denied.Reason, operatorLog) {
		t.Fatalf("S11 denied-credentials = %+v, want a failure naming the Operator's log", denied)
	}
	if c := readDeniedRecord(t, cfg.EvidenceDir).Credentials.Logs[0]; !c.Found {
		t.Fatalf("log = %+v, want the denied key found", c)
	}
}

func TestS11InterruptedUploadFailsOnAKeyInTheInstanceManagersLog(t *testing.T) {
	cfg := testS11InterruptedConfig(t)
	f := newS11InterruptedFixture(t, cfg)
	f.logs[memberLogKey(cfg, s11InterruptedMember)] = "retry put with " + testSecretKey + "\n"
	interrupted, _ := interruptedOf(t, f)
	if interrupted.Result != evidence.Fail || !strings.Contains(interrupted.Reason, instanceManagerLog) {
		t.Fatalf("S11 interrupted-upload = %+v, want a failure naming the Instance Manager's log", interrupted)
	}
	if c := readInterruptedRecord(t, cfg.EvidenceDir).Credentials.Logs[1]; !c.Found || c.Pod != s11InterruptedMember {
		t.Fatalf("log = %+v, want the key found in the member's log", c)
	}
}
