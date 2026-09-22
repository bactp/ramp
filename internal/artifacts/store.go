// Package artifacts wraps the shared checkpoint artifact store. Scenario 1
// reuses the store the Transition Operator and checkpoint-agent already agree
// on: a MinIO bucket on the management cluster, written by the node-local
// checkpoint-agent and read back by whoever needs the artifact.
//
// RAMP OBSERVES the container-checkpoint half of this store: the upload belongs
// to checkpoint-agent and the promotion-to-OCI half belongs to the Transition
// Operator. RAMP does write ONE class of object itself -- the epoch-specific
// Redis RDB snapshot -- because no existing agent produces it and because a
// RecoveryPoint that points at a live replica is not a recovery point at all.
// It never deletes anything.
package artifacts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// Put uploads an epoch artifact RAMP produced itself and returns its sha256.
// The key must be epoch-unique: an artifact that can be overwritten is not an
// immutable recovery point.
func (s *Store) Put(ctx context.Context, key string, data []byte, contentType string) (string, error) {
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	if _, ok, err := s.Stat(ctx, key); err == nil && ok {
		return "", fmt.Errorf("artifact %s/%s already exists; epoch artifacts are never overwritten", s.bucket, key)
	}
	_, err := s.client.PutObject(ctx, s.bucket, key, bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{ContentType: contentType, UserMetadata: map[string]string{"Ramp-Sha256": digest}})
	if err != nil {
		return "", fmt.Errorf("uploading %s/%s: %w", s.bucket, key, err)
	}
	return digest, nil
}

// Get reads an artifact back, verifying the sha256 when one is supplied.
func (s *Store) Get(ctx context.Context, key, wantSHA256 string) ([]byte, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("get %s/%s: %w", s.bucket, key, err)
	}
	defer obj.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(obj); err != nil {
		return nil, fmt.Errorf("reading %s/%s: %w", s.bucket, key, err)
	}
	sum := sha256.Sum256(buf.Bytes())
	got := hex.EncodeToString(sum[:])
	if wantSHA256 != "" && got != wantSHA256 {
		return nil, fmt.Errorf("artifact %s/%s checksum mismatch: want %s got %s", s.bucket, key, wantSHA256, got)
	}
	return buf.Bytes(), nil
}

// Ref renders the canonical reference recorded in a RecoveryPoint artifact.
func (s *Store) Ref(key string) string { return fmt.Sprintf("minio://%s/%s", s.bucket, key) }
