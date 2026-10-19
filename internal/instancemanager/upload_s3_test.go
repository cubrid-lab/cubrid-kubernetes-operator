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
	"crypto/rand"
	"encoding/hex"
	"net"
	"os"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
)

// TestUploadBackup_RealS3 is opt-in and permits only an explicitly selected
// loopback service. Its fresh bucket belongs to this test and is removed.
// This proves object-store behavior, not a CUBRID/PVC/VM scenario (#88).
func TestUploadBackup_RealS3(t *testing.T) {
	endpoint := os.Getenv("CUBRID_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("set CUBRID_TEST_S3_ENDPOINT for a dedicated loopback S3 service")
	}
	host, _, err := net.SplitHostPort(endpoint)
	ip := net.ParseIP(host)
	if err != nil || ip == nil || !ip.IsLoopback() {
		t.Fatal("S3 integration requires an explicit loopback IP:port")
	}
	store, err := NewMinioObjectStore(ObjectStoreConfig{Endpoint: endpoint, Region: "us-east-1",
		AccessKey: os.Getenv("CUBRID_TEST_S3_ACCESS_KEY"), SecretKey: os.Getenv("CUBRID_TEST_S3_SECRET_KEY")})
	if err != nil {
		t.Fatal("could not configure the selected integration service")
	}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		t.Fatal(err)
	}
	bucket := "cubrid-337-" + hex.EncodeToString(token[:])
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := store.client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{Region: "us-east-1"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for object := range store.client.ListObjects(cleanup, bucket, minio.ListObjectsOptions{Recursive: true}) {
			if object.Err != nil {
				t.Errorf("list test-owned bucket: %v", object.Err)
				return
			}
			if err := store.client.RemoveObject(cleanup, bucket, object.Key, minio.RemoveObjectOptions{}); err != nil {
				t.Errorf("remove test-owned object: %v", err)
			}
		}
		if err := store.client.RemoveBucket(cleanup, bucket); err != nil {
			t.Errorf("remove test-owned bucket: %v", err)
		}
	})
	t.Run("concurrent-isolated-attempts", func(t *testing.T) { testConcurrentUploads(t, store, bucket) })
	t.Run("lost-manifest-response", func(t *testing.T) { testLostManifestResponse(t, store, bucket) })
}
