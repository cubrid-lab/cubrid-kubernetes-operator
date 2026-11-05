//go:build e2e
// +build e2e

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

package e2e

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/utils"
)

func failedLoad() error {
	loadErr := &utils.ImageLoadError{Image: "im:e2e", Attempts: []string{"a1", "a2", "a3"}}
	return fmt.Errorf("could not load the Instance Manager image on Kind: %w", loadErr)
}

// S00's own record must agree with its summary entry: a load that failed in
// every attempt is blocked in both, not "fail" in the record.
func TestS00RecordIsBlockedWhenTheImageCouldNotBeLoaded(t *testing.T) {
	record := &s00Evidence{Scenario: "S00", Result: string(evidence.Fail)}
	if record.blockByImageLoad(failedLoad()) == nil {
		t.Fatal("the failed load was not recognized")
	}
	if record.Result != string(evidence.Blocked) {
		t.Errorf("record result = %q, want %q", record.Result, evidence.Blocked)
	}
	if !strings.Contains(record.Reason, "environment_image_load_failed") {
		t.Errorf("the record's reason does not name the image load: %q", record.Reason)
	}

	other := &s00Evidence{Scenario: "S00", Result: string(evidence.Fail)}
	if other.blockByImageLoad(errors.New("could not build the Instance Manager image")) != nil ||
		other.blockByImageLoad(nil) != nil || other.Result != string(evidence.Fail) || other.Reason != "" {
		t.Errorf("a failure that is not a failed load must leave the record alone: %+v", other)
	}
}

// A step of the HA group fails in its setup with the load error in its
// message; only such a step is blocked by the image load, not any other.
func TestStepFailedWithTheImageLoadError(t *testing.T) {
	imageErr := failedLoad()
	msg := "Expected success, but got an error:\n    <*utils.ImageLoadError>: " +
		utils.ImageLoadProblem + ` "im:e2e" into Kind after 3 attempt(s)`
	if blockedByImageLoad(imageErr, msg) == nil {
		t.Error("a step that failed with the load error was not recognized")
	}
	if blockedByImageLoad(imageErr, "Expected the Pod to be Ready") != nil {
		t.Error("a step that failed for another reason was taken for an image load failure")
	}
	if blockedByImageLoad(nil, msg) != nil {
		t.Error("without a failed load, no step is blocked by it")
	}
	reason := imageLoadReason(blockedByImageLoad(imageErr, msg), imageLoadEvidence)
	if !strings.HasPrefix(reason, "environment_image_load_failed") || !strings.Contains(reason, imageLoadEvidence) {
		t.Errorf("reason = %q", reason)
	}
}
