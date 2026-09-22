// Package artifacts wraps the shared checkpoint artifact store. Scenario 1
// reuses the store the Transition Operator and checkpoint-agent already agree
// on: a MinIO bucket on the management cluster, written by the node-local
// checkpoint-agent and read back by whoever needs the artifact.
//
// RAMP only ever OBSERVES this store. It never uploads and never deletes: the
// upload half belongs to checkpoint-agent and the promotion-to-OCI half
// belongs to the Transition Operator.
package artifacts

import (
	"context"
	"fmt"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Store is a read-only view of the checkpoint bucket.
type Store struct {
	client *minio.Client
	bucket string
}

// Config describes how to reach the store.
type Config struct {
	Endpoint  string // host:port, no scheme
	AccessKey string
	SecretKey string
	Bucket    string
	UseSSL    bool
}

// ObjectInfo is what a readiness check needs to know about an artifact.
type ObjectInfo struct {
	Key          string
	SizeBytes    int64
	LastModified time.Time
	ETag         string
}

// New opens a store client.
func New(cfg Config) (*Store, error) {
	c, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("minio client for %s: %w", cfg.Endpoint, err)
	}
	return &Store{client: c, bucket: cfg.Bucket}, nil
}

// Bucket is the bucket this store reads.
func (s *Store) Bucket() string { return s.bucket }

// Stat reports whether an object exists, and its size.
func (s *Store) Stat(ctx context.Context, key string) (*ObjectInfo, bool, error) {
	oi, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("stat %s/%s: %w", s.bucket, key, err)
	}
	return &ObjectInfo{
		Key: oi.Key, SizeBytes: oi.Size, LastModified: oi.LastModified, ETag: oi.ETag,
	}, true, nil
}

// WaitFor blocks until the object appears or the context/timeout expires. This
// is the CAPTURE stage's hand-off point: the kubelet has written the tar to the
// node, and the checkpoint-agent has to land it in the shared store before the
// artifact counts as captured.
func (s *Store) WaitFor(ctx context.Context, key string, timeout, poll time.Duration) (*ObjectInfo, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		oi, ok, err := s.Stat(ctx, key)
		if err != nil {
			lastErr = err
		}
		if ok {
			return oi, nil
		}
		if time.Now().After(deadline) {
			if lastErr != nil {
				return nil, fmt.Errorf("artifact %s did not land in %s within %s (last error: %v)", key, s.bucket, timeout, lastErr)
			}
			return nil, fmt.Errorf("artifact %s did not land in %s within %s", key, s.bucket, timeout)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(poll):
		}
	}
}

// Ref renders the canonical reference recorded in a RecoveryPoint artifact.
func (s *Store) Ref(key string) string { return fmt.Sprintf("minio://%s/%s", s.bucket, key) }
