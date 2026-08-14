// Package repo abstracts the places backups are written to: a local
// filesystem (including anything mounted there, such as NFS), S3-compatible
// object storage, and SFTP.
//
// The interface is deliberately narrow. Everything the backup engine needs is
// "write an object", "read part of an object" and "list/delete by prefix" —
// which is the intersection of what all three backends do well. Ranged reads
// in particular are what makes verification and restore possible without
// pulling a whole terabyte image down first.
package repo

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/google/uuid"

	"adveng/jh_virt/internal/model"
)

// ErrNotExist is returned by Get, GetRange and Stat for a missing object.
var ErrNotExist = errors.New("объект не найден в хранилище")

// ObjectInfo describes one stored object.
type ObjectInfo struct {
	Key      string    `json:"key"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
	ETag     string    `json:"etag,omitempty"`
}

// Backend is a backup repository.
//
// Implementations must be safe for concurrent use: several disks of one VM are
// transferred in parallel.
type Backend interface {
	// Kind identifies the backend type.
	Kind() model.StorageKind
	// Name is the operator-facing name of the configured target.
	Name() string

	// Put stores an object. size may be -1 when unknown, though backends that
	// need it (S3 without multipart) will then buffer. Returns bytes written.
	Put(ctx context.Context, key string, r io.Reader, size int64) (int64, error)

	// Get opens the whole object for reading.
	Get(ctx context.Context, key string) (io.ReadCloser, error)

	// GetRange opens [offset, offset+length) of the object. A length of -1
	// reads to the end.
	GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error)

	// Stat returns metadata without transferring the body.
	Stat(ctx context.Context, key string) (ObjectInfo, error)

	// Delete removes one object. Deleting a missing object is not an error:
	// retention runs must be idempotent.
	Delete(ctx context.Context, key string) error

	// DeletePrefix removes every object under a prefix and returns the count.
	DeletePrefix(ctx context.Context, prefix string) (int, error)

	// List enumerates objects under a prefix.
	List(ctx context.Context, prefix string) ([]ObjectInfo, error)

	// Usage reports free and used bytes. Backends that cannot tell return
	// (0, 0, nil) — the caller treats that as "unknown", not as "full".
	Usage(ctx context.Context) (free int64, used int64, err error)

	// Check verifies the target is reachable and writable, by round-tripping a
	// small probe object.
	Check(ctx context.Context) error

	// Close releases connections.
	Close() error
}

// optimizedCopier is implemented by a backend that can copy an object without
// routing its body through the application. The bool reports whether the
// optimization was applicable; callers then fall back to Get+Put.
type optimizedCopier interface {
	copyFrom(ctx context.Context, source Backend, key string, size int64) (written int64, applicable bool, err error)
}

// CopyOptimized attempts a storage-native object copy.
func CopyOptimized(ctx context.Context, source, destination Backend, key string, size int64) (int64, bool, error) {
	c, ok := destination.(optimizedCopier)
	if !ok {
		return 0, false, nil
	}
	return c.copyFrom(ctx, source, key, size)
}

// ObjectLockValidator is implemented by S3 targets configured for immutable
// backup objects. It never enables bucket lock implicitly.
type ObjectLockValidator interface {
	CheckObjectLock(ctx context.Context) error
}

// Open builds a backend from a stored target definition.
func Open(ctx context.Context, target *model.StorageTarget) (Backend, error) {
	var (
		backend Backend
		err     error
	)
	switch target.Kind {
	case model.StorageLocal:
		backend, err = newLocal(target)
	case model.StorageS3:
		backend, err = newS3(ctx, target)
	case model.StorageSFTP:
		backend, err = newSFTP(target)
	default:
		return nil, errors.New("неизвестный тип хранилища: " + string(target.Kind))
	}
	if err != nil {
		return nil, err
	}
	if target.RateLimit > 0 {
		backend = newRateLimitedBackend(backend, target.RateLimit)
	}
	return backend, nil
}

// probePrefix is where Check writes its scratch object. A dedicated prefix
// keeps a half-finished probe from ever looking like backup data.
const probePrefix = ".jhvirt/health-probe-"

// runCheck is the shared Check implementation: write, read back, compare,
// delete. Anything less does not prove the target is actually usable.
//
// The probe object name is unique per call. A fixed name collides whenever two
// checks overlap — which they routinely do, because saving a storage target
// triggers one while the periodic scan is running another — and the loser
// fails with a confusing permission error from the underlying rename.
func runCheck(ctx context.Context, b Backend) error {
	probeKey := probePrefix + uuid.NewString()
	payload := []byte("ovirt-backup health probe " + time.Now().UTC().Format(time.RFC3339Nano))

	if _, err := b.Put(ctx, probeKey, bytesReader(payload), int64(len(payload))); err != nil {
		return err
	}
	defer func() { _ = b.Delete(ctx, probeKey) }()

	rc, err := b.Get(ctx, probeKey)
	if err != nil {
		return err
	}
	defer rc.Close()

	got, err := io.ReadAll(io.LimitReader(rc, int64(len(payload))+16))
	if err != nil {
		return err
	}
	if string(got) != string(payload) {
		return errors.New("хранилище вернуло не то, что было записано")
	}
	return nil
}
