// Command rampctl is the small operator-side companion to ramp-manager.
//
// It exists for one reason: the recovery scripts run on the management host and
// must fetch the epoch's IMMUTABLE artifacts out of the shared MinIO store
// before they can restore anything. Re-implementing S3 signing in bash to do
// that would be a second, unverified copy of code the manager already has.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/dcn-ssu/ramp/internal/artifacts"
)

func main() {
	var (
		endpoint = flag.String("artifact-store-endpoint", "192.168.28.158:32000", "MinIO host:port.")
		bucket   = flag.String("artifact-store-bucket", "checkpoints", "Artifact store bucket.")
		key      = flag.String("key", "", "Artifact key, or a full minio://bucket/key reference.")
		out      = flag.String("out", "", "Write the artifact here ('-' for stdout).")
		sha      = flag.String("sha256", "", "Expected sha256; the fetch fails if it does not match.")
	)
	flag.Parse()

	if *key == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "usage: rampctl --key <artifact|minio://bucket/key> --out <file> [--sha256 <digest>]")
		os.Exit(2)
	}
	k := *key
	if len(k) > 8 && k[:8] == "minio://" {
		rest := k[8:]
		for i := 0; i < len(rest); i++ {
			if rest[i] == '/' {
				*bucket, k = rest[:i], rest[i+1:]
				break
			}
		}
	}

	store, err := artifacts.New(artifacts.Config{
		Endpoint: *endpoint, Bucket: *bucket,
		AccessKey: os.Getenv("MINIO_ACCESS_KEY"), SecretKey: os.Getenv("MINIO_SECRET_KEY"),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "artifact store:", err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	data, err := store.Get(ctx, k, *sha)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fetch:", err)
		os.Exit(1)
	}
	if *out == "-" {
		os.Stdout.Write(data)
		return
	}
	if err := os.WriteFile(*out, data, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "write:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "wrote %d bytes of %s/%s to %s\n", len(data), *bucket, k, *out)
}
