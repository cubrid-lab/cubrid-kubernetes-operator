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

package instancemanager

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

// boundedObjectKey is the object a bounded-read test lists in its manifest.
const boundedObjectKey = "backup/vol"

// endlessStream is an object body far longer than its manifest says: it counts
// the bytes a reader took from it and whether the reader closed it. A test
// ends it well beyond what the code may read, so that a read past the bound
// fails an assertion instead of hanging the test.
type endlessStream struct {
	read   int64
	limit  int64 // the stream ends after this many bytes; 0 means never
	closed bool
}

func (s *endlessStream) Read(p []byte) (int, error) {
	if s.limit > 0 && s.read >= s.limit {
		return 0, io.EOF
	}
	n := len(p)
	if s.limit > 0 && int64(n) > s.limit-s.read {
		n = int(s.limit - s.read)
	}
	for i := range p[:n] {
		p[i] = ' '
	}
	s.read += int64(n)
	return n, nil
}

func (s *endlessStream) Close() error {
	s.closed = true
	return nil
}

// streamingStore answers a read of the key ending in streamKey with stream and
// every other read from the fake store.
type streamingStore struct {
	*fakeObjectStore
	streamKey string
	stream    *endlessStream
	gets      int
}

func (s *streamingStore) Get(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	s.gets++
	if strings.HasSuffix(key, s.streamKey) {
		return s.stream, nil
	}
	return s.fakeObjectStore.Get(ctx, bucket, key)
}

// boundsBucket and boundsPrefix locate the artifact of a bounded-read test.
const boundsBucket, boundsPrefix = "cubrid-backups", "prod/example/uid-1"

// putRawManifest stores a manifest with the given objects as they are, without
// storing any object.
func putRawManifest(t *testing.T, store *fakeObjectStore, objects []ManifestObject) {
	t.Helper()
	bucket, prefix := boundsBucket, boundsPrefix
	m := BackupManifest{
		ManifestVersion: ManifestVersion, Database: dbName, CubridVersion: testCubridVersion,
		SourceRole: string(RoleSlave), Objects: objects,
	}
	data, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	store.objects[bucket+"/"+path.Join(prefix, ManifestObjectName)] = data
}

func checksumOf(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// A manifest.json larger than MaxManifestBytes is refused after reading at
// most one byte more than the limit, so a store that returns an endless or
// huge body cannot fill the Instance Manager's memory before the manifest is
// parsed (#842).
func TestRestore_RefusesAManifestLargerThanTheLimitAfterABoundedRead(t *testing.T) {
	const bucket, prefix = boundsBucket, boundsPrefix
	stream := &endlessStream{limit: MaxManifestBytes + 4<<20}
	store := &streamingStore{fakeObjectStore: newFakeStore(), streamKey: ManifestObjectName, stream: stream}
	req := baseRestoreRequest(t, bucket, prefix)
	cli := &restoreCLI{}

	_, err := Restore(context.Background(), cli, store, rootsFor(req), req)
	if err == nil {
		t.Fatal("a manifest larger than the limit was accepted")
	}
	if stream.read > MaxManifestBytes+1 {
		t.Errorf("read %d bytes of the manifest, want at most %d", stream.read, MaxManifestBytes+1)
	}
	if !strings.Contains(err.Error(), "larger than") {
		t.Errorf("error %q does not say the manifest is larger than the limit", err)
	}
	if !stream.closed {
		t.Error("the manifest stream was not closed")
	}
	if len(cli.calls) != 0 {
		t.Errorf("restoredb ran: %v", cli.calls)
	}
}

func TestVerifyArtifact_RefusesAManifestLargerThanTheLimitAfterABoundedRead(t *testing.T) {
	stream := &endlessStream{limit: MaxManifestBytes + 4<<20}
	store := &streamingStore{fakeObjectStore: newFakeStore(), streamKey: ManifestObjectName, stream: stream}
	if _, err := VerifyArtifact(context.Background(), store, "b", "p", goodManifestExpectation()); err == nil {
		t.Fatal("a manifest larger than the limit was accepted")
	}
	if stream.read > MaxManifestBytes+1 {
		t.Errorf("read %d bytes of the manifest, want at most %d", stream.read, MaxManifestBytes+1)
	}
}

// An object whose manifest entry declares one byte but whose body goes on is
// refused after at most two bytes: the restore neither fills the staging
// volume with the rest nor waits for the body to end. Only the incomplete
// staged file is removed, other files in the staging directory stay, and
// restoredb does not run (#842).
func TestRestore_StopsReadingAnObjectThatIsLongerThanItsManifestSize(t *testing.T) {
	const bucket, prefix = boundsBucket, boundsPrefix
	fake := newFakeStore()
	putRawManifest(t, fake, []ManifestObject{{Key: boundedObjectKey, SizeBytes: 1, SHA256: checksumOf(" ")}})
	stream := &endlessStream{limit: 8 << 20}
	store := &streamingStore{fakeObjectStore: fake, streamKey: boundedObjectKey, stream: stream}
	req := baseRestoreRequest(t, bucket, prefix)
	// A staging directory that already exists is not this restore's to remove.
	if err := os.MkdirAll(req.StagingDir, 0o750); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(req.StagingDir, "other")
	if err := os.WriteFile(other, []byte("kept"), 0o600); err != nil {
		t.Fatal(err)
	}
	cli := &restoreCLI{}

	_, err := Restore(context.Background(), cli, store, rootsFor(req), req)
	if err == nil {
		t.Fatal("an object longer than its manifest size was restored")
	}
	if stream.read > 2 {
		t.Errorf("read %d bytes of a 1-byte object, want at most 2", stream.read)
	}
	if !stream.closed {
		t.Error("the object stream was not closed")
	}
	if _, statErr := os.Stat(filepath.Join(req.StagingDir, "backup", "vol")); !os.IsNotExist(statErr) {
		t.Errorf("the incomplete staged file is still there (err=%v)", statErr)
	}
	if data, _ := os.ReadFile(other); string(data) != "kept" {
		t.Errorf("another file in the staging directory was changed to %q", data)
	}
	if len(cli.calls) != 0 {
		t.Errorf("restoredb ran: %v", cli.calls)
	}
}

func TestVerifyArtifact_StopsReadingAnObjectThatIsLongerThanItsManifestSize(t *testing.T) {
	const bucket, prefix = boundsBucket, boundsPrefix
	fake := newFakeStore()
	putRawManifest(t, fake, []ManifestObject{{Key: boundedObjectKey, SizeBytes: 1, SHA256: checksumOf(" ")}})
	stream := &endlessStream{limit: 8 << 20}
	store := &streamingStore{fakeObjectStore: fake, streamKey: boundedObjectKey, stream: stream}
	if _, err := VerifyArtifact(context.Background(), store, bucket, prefix, goodManifestExpectation()); err == nil {
		t.Fatal("an object longer than its manifest size was accepted")
	}
	if stream.read > 2 {
		t.Errorf("read %d bytes of a 1-byte object, want at most 2", stream.read)
	}
}

// A manifest whose object list cannot describe a real backup is refused when
// it is parsed, before any object is downloaded (#842): a negative size, more
// objects than MaxManifestObjects, sizes whose sum does not fit in 64 bits,
// and a checksum that is not a lowercase hex SHA-256.
func TestRestore_RefusesAnImpossibleObjectListBeforeDownloading(t *testing.T) {
	sum := checksumOf(backupContent)
	tooMany := make([]ManifestObject, MaxManifestObjects+1)
	for i := range tooMany {
		tooMany[i] = ManifestObject{Key: fmt.Sprintf("backup/v%d", i), SizeBytes: 1, SHA256: sum}
	}
	cases := map[string][]ManifestObject{
		"negative size":    {{Key: boundedObjectKey, SizeBytes: -1, SHA256: sum}},
		"too many objects": tooMany,
		"total size overflows": {
			{Key: "backup/a", SizeBytes: math.MaxInt64, SHA256: sum},
			{Key: "backup/b", SizeBytes: 1, SHA256: sum},
		},
		"checksum not hex":        {{Key: boundedObjectKey, SizeBytes: 1, SHA256: strings.Repeat("z", 64)}},
		"checksum wrong length":   {{Key: boundedObjectKey, SizeBytes: 1, SHA256: "abc"}},
		"checksum upper-case hex": {{Key: boundedObjectKey, SizeBytes: 1, SHA256: strings.ToUpper(sum)}},
	}
	for name, objects := range cases {
		t.Run(name, func(t *testing.T) {
			const bucket, prefix = boundsBucket, boundsPrefix
			fake := newFakeStore()
			putRawManifest(t, fake, objects)
			data := fake.objects[bucket+"/"+path.Join(prefix, ManifestObjectName)]
			if _, err := ParseManifest(data); err == nil {
				t.Error("ParseManifest accepted the manifest")
			}

			store := &countingStore{fakeObjectStore: fake}
			req := baseRestoreRequest(t, bucket, prefix)
			cli := &restoreCLI{}
			if _, err := Restore(context.Background(), cli, store, rootsFor(req), req); err == nil {
				t.Fatal("the manifest was restored")
			}
			if store.gets != 1 {
				t.Errorf("the store was read %d times, want only the manifest", store.gets)
			}
			if len(cli.calls) != 0 {
				t.Errorf("restoredb ran: %v", cli.calls)
			}

			store.gets = 0
			if _, err := VerifyArtifact(context.Background(), store, bucket, prefix, goodManifestExpectation()); err == nil {
				t.Fatal("VerifyArtifact accepted the manifest")
			}
			if store.gets != 1 {
				t.Errorf("verification read the store %d times, want only the manifest", store.gets)
			}
		})
	}
}

// An ordinary backup of several objects, one of them larger than a read
// buffer, still restores and verifies with the bounds in place.
func TestRestore_RestoresAndVerifiesAMultiFileBackupWithinTheBounds(t *testing.T) {
	const bucket, prefix = boundsBucket, boundsPrefix
	fake := newFakeStore()
	files := map[string]string{
		"backup/" + stagedFileName: backupContent,
		"backup/pocdb_bk0v001":     strings.Repeat("v", 3<<20),
		"backup/pocdb_bkvinf":      "info",
	}
	objects := make([]ManifestObject, 0, len(files))
	for key, content := range files {
		objects = append(objects, ManifestObject{Key: key, SizeBytes: int64(len(content)), SHA256: checksumOf(content)})
		fake.objects[bucket+"/"+path.Join(prefix, key)] = []byte(content)
	}
	putRawManifest(t, fake, objects)

	if _, err := VerifyArtifact(context.Background(), fake, bucket, prefix, goodManifestExpectation()); err != nil {
		t.Fatalf("VerifyArtifact: %v", err)
	}
	req := baseRestoreRequest(t, bucket, prefix)
	staged := map[string]string{}
	cli := &restoreCLI{onRun: func() {
		for key := range files {
			data, _ := os.ReadFile(filepath.Join(req.StagingDir, filepath.FromSlash(key)))
			staged[key] = string(data)
		}
	}}
	if _, err := Restore(context.Background(), cli, fake, rootsFor(req), req); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	for key, content := range files {
		if staged[key] != content {
			t.Errorf("staged %s has %d bytes, want %d", key, len(staged[key]), len(content))
		}
	}
}
