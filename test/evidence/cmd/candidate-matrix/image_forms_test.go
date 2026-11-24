package main

import (
	"strings"
	"testing"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

// A real installed run records each image as the Pod imageID,
// "<registry/repository>@sha256:<hex>". Only the digest is compared with the
// candidate's bare digest, so such a run is matching and its results are judged.
func TestVerifierAcceptsRepositoryQualifiedImageDigests(t *testing.T) {
	operator := "ghcr.io/cubrid-lab/cubrid-kubernetes-operator/manager@" + candidateOperator
	root, manifest := buildCandidate(t, func(runs []evidence.Summary) {
		runs[1].Environment.OperatorImageDigest = operator
		runs[1].Environment.CubridImageDigest = "localhost:5000/cubrid/cubrid@" + candidateCUBRID
	})
	out, ok := runVerifier(t, root, manifest, v01Families)
	if !ok || !strings.Contains(out, overallGo) {
		t.Fatalf("a run recording repository-qualified digests did not verify as Go:\n%s", out)
	}
	if row := requiredRow(t, out, powerOffKey); !strings.Contains(row, "| GO |") {
		t.Errorf("S04 of the qualified run is not Go: %s", row)
	}
	if !strings.Contains(out, "| candidate-vm-power-off-run | vm-lab | lab-installed-ha-s04 | MATCHING CANDIDATE |") {
		t.Errorf("the report does not show the qualified run as matching:\n%s", out)
	}
}

// Anything that is not exactly a bare digest or a repository path followed by
// one "@sha256:<64 lowercase hex>" records no digest; a well-formed digest of
// another image is not the candidate's.
func TestVerifierBlocksMalformedOrOtherQualifiedImageDigests(t *testing.T) {
	repo := "ghcr.io/cubrid-lab/cubrid-kubernetes-operator/manager"
	unrecorded := "image digests are not recorded"
	tests := []struct{ name, image, want string }{
		{"tag only", repo + ":v0.1.0", unrecorded},
		{"short digest", repo + "@" + candidateOperator[:len(candidateOperator)-1], unrecorded},
		{"uppercase digest", repo + "@sha256:" + strings.Repeat("A", 64), unrecorded},
		{"uppercase repository", "GHCR.io/Cubrid/manager@" + candidateOperator, unrecorded},
		{"text after the hex", repo + "@" + candidateOperator + "extra", unrecorded},
		{"two separators", repo + "@" + repo + "@" + candidateOperator, unrecorded},
		{"no repository path", "manager@" + candidateOperator, unrecorded},
		{"other digest", repo + "@sha256:" + strings.Repeat("5", 64), otherImages},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root, manifest := buildCandidate(t, func(runs []evidence.Summary) {
				runs[1].Environment.OperatorImageDigest = tc.image
			})
			out, ok := runVerifier(t, root, manifest, v01Families)
			if ok || !strings.Contains(out, overallNoGo) {
				t.Fatalf("not No-Go:\n%s", out)
			}
			if row := requiredRow(t, out, powerOffKey); !strings.Contains(row, "| NO-GO |") ||
				!strings.Contains(row, tc.want) {
				t.Errorf("S04 does not say %q: %s", tc.want, row)
			}
		})
	}
}
