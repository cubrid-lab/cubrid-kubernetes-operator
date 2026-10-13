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

import (
	"path"
	"strings"
	"testing"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
)

// manifestURICase is one manifestUri and whether it is accepted. The same
// table is used by the parser's unit test and by the envtest spec that checks
// the API server's admission of spec.bootstrap.recovery.manifestUri agrees.
type manifestURICase struct {
	name       string
	uri        string
	wantBucket string
	wantPrefix string
	accepted   bool
}

// uriBucket is the bucket of most of the cases.
const uriBucket = "bucket"

// maxBucket and maxPrefix make the longest URI accepted: a 63-character bucket
// and a key of 1024 characters, the S3 key limit.
var (
	maxBucket = strings.Repeat("a", 63)
	maxPrefix = strings.Repeat("k", 1024-len("/manifest.json"))
)

var manifestURICases = []manifestURICase{
	// Accepted: s3://<bucket>/<prefix>/manifest.json.
	{"operator backup layout", "s3://cubrid-backups/prod/uid/manifest.json", "cubrid-backups", "prod/uid", true},
	{"deep prefix", "s3://b12/a/b/c/manifest.json", "b12", "a/b/c", true},
	{"one-segment prefix", "s3://bucket/p/manifest.json", uriBucket, "p", true},
	{"dotted bucket", "s3://my.bucket-1/p/manifest.json", "my.bucket-1", "p", true},
	{"63-character bucket", "s3://" + strings.Repeat("a", 63) + "/p/manifest.json", strings.Repeat("a", 63), "p", true},
	{"3-character bucket", "s3://abc/p/manifest.json", "abc", "p", true},
	{"dots inside segments", "s3://bucket/.hidden/a..b/.../v1.2/manifest.json", uriBucket, ".hidden/a..b/.../v1.2", true},
	{"segment named like the manifest", "s3://bucket/manifest.json/manifest.json", uriBucket, "manifest.json", true},
	{"other key characters", "s3://bucket/a-b_c!*'()+=,;:@&$~/manifest.json", uriBucket, "a-b_c!*'()+=,;:@&$~", true},
	{"URI at the length limit", "s3://" + maxBucket + "/" + maxPrefix + "/manifest.json", maxBucket, maxPrefix, true},

	// Rejected: scheme.
	{"empty", "", "", "", false},
	{"https scheme", "https://bucket/p/manifest.json", "", "", false},
	{"upper-case scheme", "S3://bucket/p/manifest.json", "", "", false},
	{"s3a scheme", "s3a://bucket/p/manifest.json", "", "", false},
	{"missing slash after scheme", "s3:/bucket/p/manifest.json", "", "", false},
	{"no scheme", "bucket/p/manifest.json", "", "", false},
	// Rejected: bucket.
	{"empty bucket", "s3:///p/manifest.json", "", "", false},
	{"bucket only", "s3://bucket", "", "", false},
	{"scheme only", "s3://", "", "", false},
	{"2-character bucket", "s3://ab/p/manifest.json", "", "", false},
	{"64-character bucket", "s3://" + strings.Repeat("a", 64) + "/p/manifest.json", "", "", false},
	{"upper-case bucket", "s3://Bucket/p/manifest.json", "", "", false},
	{"underscore in bucket", "s3://my_bucket/p/manifest.json", "", "", false},
	{"bucket starting with a hyphen", "s3://-bucket/p/manifest.json", "", "", false},
	{"bucket ending with a hyphen", "s3://bucket-/p/manifest.json", "", "", false},
	{"bucket starting with a dot", "s3://.bucket/p/manifest.json", "", "", false},
	{"bucket ending with a dot", "s3://bucket./p/manifest.json", "", "", false},
	{"adjacent dots in bucket", "s3://my..bucket/p/manifest.json", "", "", false},
	{"user info in authority", "s3://user@bucket/p/manifest.json", "", "", false},
	{"port in authority", "s3://bucket:9000/p/manifest.json", "", "", false},
	// Rejected: file name.
	{"another file name", "s3://bucket/a/b/other.json", "", "", false},
	{"manifest with a suffix", "s3://bucket/p/manifest.json.bak", "", "", false},
	{"manifest with a prefix", "s3://bucket/p/xmanifest.json", "", "", false},
	{"upper-case file name", "s3://bucket/p/MANIFEST.JSON", "", "", false},
	{"directory only", "s3://bucket/p", "", "", false},
	// Rejected: prefix.
	{"empty prefix", "s3://bucket/manifest.json", "", "", false},
	{"parent segment", "s3://bucket/a/../manifest.json", "", "", false},
	{"leading parent segment", "s3://bucket/../p/manifest.json", "", "", false},
	{"parent segment only", "s3://bucket/../manifest.json", "", "", false},
	{"current segment", "s3://bucket/./p/manifest.json", "", "", false},
	{"current segment before the file", "s3://bucket/p/./manifest.json", "", "", false},
	{"empty segment", "s3://bucket/a//manifest.json", "", "", false},
	{"empty leading segment", "s3://bucket//p/manifest.json", "", "", false},
	{"trailing slash", "s3://bucket/p/", "", "", false},
	{"trailing slash after the file", "s3://bucket/p/manifest.json/", "", "", false},
	// Rejected: characters with another meaning in a URI, or none in a key.
	{"query", "s3://bucket/p/manifest.json?versionId=1", "", "", false},
	{"query in the prefix", "s3://bucket/p?x=1/manifest.json", "", "", false},
	{"fragment", "s3://bucket/p/manifest.json#x", "", "", false},
	{"percent escape", "s3://bucket/a%2F../manifest.json", "", "", false},
	{"space", "s3://bucket/a b/manifest.json", "", "", false},
	{"tab", "s3://bucket/a\tb/manifest.json", "", "", false},
	{"newline", "s3://bucket/p/manifest.json\n", "", "", false},
	{"URI over the length limit", "s3://" + maxBucket + "/k" + maxPrefix + "/manifest.json", "", "", false},
}

func TestParseManifestURI(t *testing.T) {
	for _, tc := range manifestURICases {
		t.Run(tc.name, func(t *testing.T) {
			bucket, prefix, err := parseManifestURI(tc.uri)
			if !tc.accepted {
				if err == nil {
					t.Fatalf("parseManifestURI(%q) = %q,%q, want an error", tc.uri, bucket, prefix)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseManifestURI(%q): %v", tc.uri, err)
			}
			if bucket != tc.wantBucket || prefix != tc.wantPrefix {
				t.Fatalf("parseManifestURI(%q) = %q,%q want %q,%q", tc.uri, bucket, prefix, tc.wantBucket, tc.wantPrefix)
			}
			// The Instance Manager reads path.Join(prefix, manifest.json):
			// that must be the key the URI names, not a normalized one.
			if got := "s3://" + bucket + "/" + path.Join(prefix, instancemanager.ManifestObjectName); got != tc.uri {
				t.Fatalf("the Instance Manager would read %q for %q", got, tc.uri)
			}
		})
	}
}
