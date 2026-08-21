package replication

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/repo"
)

// TestExternalBackendMatrix is opt-in because it needs MinIO and SFTP. The
// repository test runner can provide the endpoints locally or in CI.
func TestExternalBackendMatrix(t *testing.T) {
	endpoint := os.Getenv("JHV_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("JHV_TEST_S3_ENDPOINT is not set")
	}
	port, err := strconv.Atoi(os.Getenv("JHV_TEST_SFTP_PORT"))
	if err != nil {
		t.Fatal("JHV_TEST_SFTP_PORT must be an integer")
	}
	ctx := context.Background()
	targets := map[string]*model.StorageTarget{
		"local": {Name: "local", Kind: model.StorageLocal, BasePath: t.TempDir(), Enabled: true},
		"s3a": {Name: "s3-a", Kind: model.StorageS3, Endpoint: endpoint,
			Bucket: os.Getenv("JHV_TEST_S3_BUCKET"), Prefix: "a", AccessKey: os.Getenv("JHV_TEST_S3_ACCESS_KEY"),
			SecretKey: os.Getenv("JHV_TEST_S3_SECRET_KEY"), PathStyle: true, UseSSL: false, Enabled: true},
		"s3b": {Name: "s3-b", Kind: model.StorageS3, Endpoint: endpoint,
			Bucket: os.Getenv("JHV_TEST_S3_BUCKET"), Prefix: "b", AccessKey: os.Getenv("JHV_TEST_S3_ACCESS_KEY"),
			SecretKey: os.Getenv("JHV_TEST_S3_SECRET_KEY"), PathStyle: true, UseSSL: false, Enabled: true},
		"sftp": {Name: "sftp", Kind: model.StorageSFTP, Host: os.Getenv("JHV_TEST_SFTP_HOST"),
			Port: port, Username: os.Getenv("JHV_TEST_SFTP_USER"), Password: os.Getenv("JHV_TEST_SFTP_PASSWORD"),
			BasePath: os.Getenv("JHV_TEST_SFTP_PATH"), Enabled: true},
	}
	backends := map[string]repo.Backend{}
	for name, target := range targets {
		backend, err := repo.Open(ctx, target)
		if err != nil {
			t.Fatalf("open %s: %v", name, err)
		}
		if err := backend.Check(ctx); err != nil {
			t.Fatalf("check %s: %v", name, err)
		}
		backends[name] = backend
		defer backend.Close()
	}

	directions := [][2]string{{"local", "s3a"}, {"s3a", "s3b"}, {"s3b", "sftp"},
		{"sftp", "local"}, {"local", "sftp"}, {"sftp", "s3a"}, {"s3a", "local"}}
	for _, direction := range directions {
		sourceName, destinationName := direction[0], direction[1]
		t.Run(sourceName+"_to_"+destinationName, func(t *testing.T) {
			key := fmt.Sprintf("integration/%s-to-%s.bin", sourceName, destinationName)
			payload := bytes.Repeat([]byte(sourceName+"->"+destinationName+"\n"), 1024)
			if _, err := backends[sourceName].Put(ctx, key, bytes.NewReader(payload), int64(len(payload))); err != nil {
				t.Fatal(err)
			}
			hash, err := copyOne(ctx, backends[sourceName], backends[destinationName], repo.ObjectInfo{Key: key, Size: int64(len(payload))})
			if err != nil {
				t.Fatal(err)
			}
			expectedHash := fmt.Sprintf("%x", sha256.Sum256(payload))
			if hash != expectedHash {
				t.Fatalf("hash %s, want %s", hash, expectedHash)
			}
			rc, err := backends[destinationName].Get(ctx, key)
			if err != nil {
				t.Fatal(err)
			}
			actual, err := io.ReadAll(rc)
			_ = rc.Close()
			if err != nil || !bytes.Equal(actual, payload) {
				t.Fatalf("destination differs: bytes=%d err=%v", len(actual), err)
			}
		})
	}

	lockedPrefix := fmt.Sprintf("locked-integration-%d", time.Now().UnixNano())
	locked := &model.StorageTarget{Name: "s3-locked", Kind: model.StorageS3, Endpoint: endpoint,
		Bucket: os.Getenv("JHV_TEST_S3_LOCKED_BUCKET"), AccessKey: os.Getenv("JHV_TEST_S3_ACCESS_KEY"),
		SecretKey: os.Getenv("JHV_TEST_S3_SECRET_KEY"), Prefix: lockedPrefix,
		PathStyle: true, UseSSL: false, Enabled: true,
		ObjectLockEnabled: true, ObjectLockDays: 1}
	lockedBackend, err := repo.Open(ctx, locked)
	if err != nil {
		t.Fatal(err)
	}
	defer lockedBackend.Close()
	validator, ok := lockedBackend.(repo.ObjectLockValidator)
	if !ok {
		t.Fatal("S3 backend does not expose Object Lock validation")
	}
	if err := validator.CheckObjectLock(ctx); err != nil {
		t.Fatal(err)
	}
	if err := lockedBackend.Check(ctx); err != nil {
		t.Fatal(err)
	}
	key := "jhvirt/integration/locked.bin"
	if _, err := lockedBackend.Put(ctx, key, bytes.NewReader([]byte("immutable")), 9); err != nil {
		t.Fatal(err)
	}
	if err := lockedBackend.Delete(ctx, key); err == nil {
		t.Fatal("Governance object was deleted before retention expired")
	}
	if _, err := lockedBackend.Stat(ctx, key); err != nil {
		t.Fatalf("locked object disappeared: %v", err)
	}

	// Versioned buckets retain data behind delete markers unless every
	// concrete version is removed. Staging and health probes must leave no
	// versions because they intentionally have no Governance retention.
	minioEndpoint := strings.TrimPrefix(strings.TrimPrefix(endpoint, "https://"), "http://")
	client, err := minio.New(minioEndpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(locked.AccessKey, locked.SecretKey, ""),
		Secure: strings.HasPrefix(endpoint, "https://"),
	})
	if err != nil {
		t.Fatal(err)
	}
	internalPrefix := lockedPrefix + "/.jhvirt/"
	for object := range client.ListObjects(ctx, locked.Bucket, minio.ListObjectsOptions{
		Prefix: internalPrefix, Recursive: true, WithVersions: true,
	}) {
		if object.Err != nil {
			t.Fatal(object.Err)
		}
		t.Fatalf("temporary Object Lock version was not removed: key=%s version=%s delete_marker=%v",
			object.Key, object.VersionID, object.IsDeleteMarker)
	}
}
