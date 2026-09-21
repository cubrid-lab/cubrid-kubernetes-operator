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

import "testing"

func TestParseManifestURI(t *testing.T) {
	tests := []struct {
		uri        string
		wantBucket string
		wantPrefix string
		wantErr    bool
	}{
		{"s3://cubrid-backups/prod/uid/manifest.json", "cubrid-backups", "prod/uid", false},
		{"s3://b/manifest.json", "b", ".", false},
		{"s3://b/a/b/c/manifest.json", "b", "a/b/c", false},
		{"https://x/manifest.json", "", "", true},
		{"s3://onlybucket", "", "", true},
		{"s3://", "", "", true},
	}
	for _, tc := range tests {
		bucket, prefix, err := parseManifestURI(tc.uri)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseManifestURI(%q) expected error", tc.uri)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseManifestURI(%q): %v", tc.uri, err)
			continue
		}
		if bucket != tc.wantBucket || prefix != tc.wantPrefix {
			t.Errorf("parseManifestURI(%q) = %q,%q want %q,%q", tc.uri, bucket, prefix, tc.wantBucket, tc.wantPrefix)
		}
	}
}
