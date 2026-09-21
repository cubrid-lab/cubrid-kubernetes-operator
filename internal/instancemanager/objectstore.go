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
	"fmt"
	"io"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// ObjectStore is the seam the backup flow uses to reach S3-compatible storage
// (ADR-0007). It is an interface so the upload path can be faked in tests
// without a live bucket.
type ObjectStore interface {
	// Put uploads size bytes from r to bucket/key and returns the stored size.
	Put(ctx context.Context, bucket, key string, r io.Reader, size int64, contentType string) (int64, error)
	// StatSize returns the stored object's size, used to verify an upload landed.
	StatSize(ctx context.Context, bucket, key string) (int64, error)
}

// ObjectStoreConfig configures an S3-compatible endpoint (ADR-0007).
type ObjectStoreConfig struct {
	Endpoint  string
	AccessKey string
	SecretKey string
	Region    string
	Secure    bool
}

// MinioObjectStore is the minio-go-backed ObjectStore.
type MinioObjectStore struct {
	client *minio.Client
}

// NewMinioObjectStore builds an S3-compatible object store client.
func NewMinioObjectStore(cfg ObjectStoreConfig) (*MinioObjectStore, error) {
	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.Secure,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("init object store client: %w", err)
	}
	return &MinioObjectStore{client: client}, nil
}

func (s *MinioObjectStore) Put(ctx context.Context, bucket, key string, r io.Reader, size int64, contentType string) (int64, error) {
	info, err := s.client.PutObject(ctx, bucket, key, r, size, minio.PutObjectOptions{ContentType: contentType})
	if err != nil {
		return 0, fmt.Errorf("put %s/%s: %w", bucket, key, err)
	}
	return info.Size, nil
}

func (s *MinioObjectStore) StatSize(ctx context.Context, bucket, key string) (int64, error) {
	info, err := s.client.StatObject(ctx, bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return 0, fmt.Errorf("stat %s/%s: %w", bucket, key, err)
	}
	return info.Size, nil
}
