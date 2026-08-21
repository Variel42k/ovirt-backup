package repo

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

// s3Backend stores backups in S3-compatible object storage: AWS S3, MinIO,
// Ceph RGW and the Russian clouds that speak the same protocol.
type s3Backend struct {
	name           string
	client         *minio.Client
	bucket         string
	prefix         string
	storageClass   string
	endpoint       string
	objectLockDays int
}

func newS3(ctx context.Context, target *model.StorageTarget) (Backend, error) {
	if target.Bucket == "" {
		return nil, errors.New("для S3-хранилища не задан bucket")
	}
	endpoint := strings.TrimSpace(target.Endpoint)
	if endpoint == "" {
		return nil, errors.New("для S3-хранилища не задан endpoint")
	}
	// minio-go wants a bare host:port, but operators paste full URLs.
	secure := target.UseSSL
	if strings.HasPrefix(endpoint, "https://") {
		endpoint, secure = strings.TrimPrefix(endpoint, "https://"), true
	} else if strings.HasPrefix(endpoint, "http://") {
		endpoint, secure = strings.TrimPrefix(endpoint, "http://"), false
	}
	endpoint = strings.TrimRight(endpoint, "/")

	opts := &minio.Options{
		Creds:  credentials.NewStaticV4(target.AccessKey, target.SecretKey, ""),
		Secure: secure,
		Region: target.Region,
		Transport: &http.Transport{
			MaxIdleConns:        64,
			MaxIdleConnsPerHost: 16,
			IdleConnTimeout:     90 * time.Second,
		},
	}
	if target.PathStyle {
		opts.BucketLookup = minio.BucketLookupPath
	}

	client, err := minio.New(endpoint, opts)
	if err != nil {
		return nil, fmt.Errorf("подключение к S3 %s: %w", endpoint, err)
	}

	return &s3Backend{
		name:         target.Name,
		client:       client,
		bucket:       target.Bucket,
		prefix:       strings.Trim(target.Prefix, "/"),
		storageClass: target.StorageClass,
		endpoint:     endpoint,
		objectLockDays: func() int {
			if target.ObjectLockEnabled {
				return target.ObjectLockDays
			}
			return 0
		}(),
	}, nil
}

func (s *s3Backend) Kind() model.StorageKind { return model.StorageS3 }
func (s *s3Backend) Name() string            { return s.name }
func (s *s3Backend) Close() error            { return nil }

func (s *s3Backend) objectKey(key string) string {
	key = strings.TrimLeft(key, "/")
	if s.prefix == "" {
		return key
	}
	return s.prefix + "/" + key
}

func (s *s3Backend) Put(ctx context.Context, key string, r io.Reader, size int64) (int64, error) {
	if s.objectLockDays > 0 && strings.HasPrefix(strings.TrimLeft(key, "/"), repoDataPrefix()) {
		return s.putLocked(ctx, key, r, size)
	}
	return s.put(ctx, key, r, size, minio.PutObjectOptions{})
}

func repoDataPrefix() string { return "jhvirt/" }

func (s *s3Backend) put(ctx context.Context, key string, r io.Reader, size int64, opts minio.PutObjectOptions) (int64, error) {
	opts.ContentType = "application/octet-stream"
	if s.storageClass != "" {
		opts.StorageClass = s.storageClass
	}
	// A size of -1 makes minio-go stream with multipart uploads, which is what
	// we want for disk data whose compressed length is not known up front.
	if size < 0 {
		opts.PartSize = 64 << 20
	}

	info, err := s.client.PutObject(ctx, s.bucket, s.objectKey(key), r, size, opts)
	if err != nil {
		return 0, fmt.Errorf("запись %s в S3: %w", key, err)
	}
	return info.Size, nil
}

// putLocked uses an unlocked staging object so a failed upload never leaves an
// immutable partial publication. Staging is read back before the final S3 copy.
func (s *s3Backend) putLocked(ctx context.Context, key string, r io.Reader, size int64) (int64, error) {
	stage := ".jhvirt/staging/" + uuid.NewString()
	h := sha256.New()
	n, err := s.put(ctx, stage, io.TeeReader(r, h), size, minio.PutObjectOptions{})
	if err != nil {
		return n, err
	}
	defer func() { _ = s.Delete(ctx, stage) }()

	rc, err := s.Get(ctx, stage)
	if err != nil {
		return 0, fmt.Errorf("чтение staging %s: %w", key, err)
	}
	check := sha256.New()
	_, readErr := io.Copy(check, rc)
	closeErr := rc.Close()
	if readErr != nil {
		return 0, fmt.Errorf("проверка staging %s: %w", key, readErr)
	}
	if closeErr != nil {
		return 0, closeErr
	}
	if fmt.Sprintf("%x", h.Sum(nil)) != fmt.Sprintf("%x", check.Sum(nil)) {
		return 0, fmt.Errorf("контрольная сумма staging %s не совпала", key)
	}

	_, err = s.client.CopyObject(ctx, minio.CopyDestOptions{
		Bucket: s.bucket, Object: s.objectKey(key),
		Mode: minio.Governance, RetainUntilDate: time.Now().UTC().AddDate(0, 0, s.objectLockDays),
	}, minio.CopySrcOptions{Bucket: s.bucket, Object: s.objectKey(stage)})
	if err != nil {
		return 0, fmt.Errorf("публикация заблокированного объекта %s: %w", key, err)
	}
	return n, nil
}

func (s *s3Backend) copyFrom(ctx context.Context, source Backend, key string, size int64) (int64, bool, error) {
	src, ok := source.(*s3Backend)
	if !ok || src.endpoint != s.endpoint {
		return 0, false, nil
	}
	if s.objectLockDays > 0 {
		stage := ".jhvirt/staging/" + uuid.NewString()
		defer func() { _ = s.Delete(ctx, stage) }()
		info, err := s.client.CopyObject(ctx,
			minio.CopyDestOptions{Bucket: s.bucket, Object: s.objectKey(stage), Size: size},
			minio.CopySrcOptions{Bucket: src.bucket, Object: src.objectKey(key)})
		if err != nil {
			// Different credentials on the same endpoint may not grant the
			// destination account read access to the source bucket. Streaming
			// through each backend's own credentials remains valid.
			return 0, false, nil
		}
		sourceHash, err := hashS3Object(ctx, source, key)
		if err != nil {
			return 0, true, err
		}
		stageHash, err := hashS3Object(ctx, s, stage)
		if err != nil {
			return 0, true, err
		}
		if sourceHash != stageHash {
			return 0, true, fmt.Errorf("SHA-256 staging объекта %s не совпал", key)
		}
		_, err = s.client.CopyObject(ctx, minio.CopyDestOptions{
			Bucket: s.bucket, Object: s.objectKey(key), Size: size,
			Mode: minio.Governance, RetainUntilDate: time.Now().UTC().AddDate(0, 0, s.objectLockDays),
		}, minio.CopySrcOptions{Bucket: s.bucket, Object: s.objectKey(stage)})
		if err != nil {
			return 0, true, fmt.Errorf("публикация заблокированной копии %s: %w", key, err)
		}
		return info.Size, true, nil
	}
	dst := minio.CopyDestOptions{Bucket: s.bucket, Object: s.objectKey(key), Size: size}
	info, err := s.client.CopyObject(ctx, dst, minio.CopySrcOptions{
		Bucket: src.bucket, Object: src.objectKey(key),
	})
	if err != nil {
		return 0, false, nil
	}
	return info.Size, true, nil
}

func hashS3Object(ctx context.Context, backend Backend, key string) (string, error) {
	rc, err := backend.Get(ctx, key)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	h := sha256.New()
	if _, err := io.Copy(h, rc); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func (s *s3Backend) CheckObjectLock(ctx context.Context) error {
	if s.objectLockDays <= 0 {
		return nil
	}
	enabled, _, _, _, err := s.client.GetObjectLockConfig(ctx, s.bucket)
	if err != nil {
		return fmt.Errorf("проверка Object Lock bucket %s: %w", s.bucket, err)
	}
	if !strings.EqualFold(enabled, "Enabled") {
		return fmt.Errorf("Object Lock не включён для bucket %q", s.bucket)
	}
	return nil
}

func (s *s3Backend) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	return s.GetRange(ctx, key, 0, -1)
}

func (s *s3Backend) GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	opts := minio.GetObjectOptions{}
	if offset > 0 || length > 0 {
		if length < 0 {
			if err := opts.SetRange(offset, 0); err != nil {
				return nil, err
			}
		} else if err := opts.SetRange(offset, offset+length-1); err != nil {
			return nil, err
		}
	}

	obj, err := s.client.GetObject(ctx, s.bucket, s.objectKey(key), opts)
	if err != nil {
		return nil, translateS3Error(key, err)
	}
	// GetObject is lazy: it does not contact the server until the first read,
	// so a missing object would otherwise only surface later, far from here.
	if _, err := obj.Stat(); err != nil {
		_ = obj.Close()
		return nil, translateS3Error(key, err)
	}
	return obj, nil
}

func (s *s3Backend) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	info, err := s.client.StatObject(ctx, s.bucket, s.objectKey(key), minio.StatObjectOptions{})
	if err != nil {
		return ObjectInfo{}, translateS3Error(key, err)
	}
	return ObjectInfo{Key: key, Size: info.Size, Modified: info.LastModified.UTC(), ETag: info.ETag}, nil
}

func (s *s3Backend) Delete(ctx context.Context, key string) error {
	if s.objectLockDays > 0 {
		_, err := s.deleteVersions(ctx, key, true)
		return err
	}
	err := s.client.RemoveObject(ctx, s.bucket, s.objectKey(key), minio.RemoveObjectOptions{})
	if err != nil && !errors.Is(translateS3Error(key, err), ErrNotExist) {
		return err
	}
	return nil
}

func (s *s3Backend) DeletePrefix(ctx context.Context, prefix string) (int, error) {
	if s.objectLockDays > 0 {
		return s.deleteVersions(ctx, prefix, false)
	}
	objects, err := s.List(ctx, prefix)
	if err != nil {
		return 0, err
	}
	if len(objects) == 0 {
		return 0, nil
	}

	ch := make(chan minio.ObjectInfo, 32)
	go func() {
		defer close(ch)
		for _, o := range objects {
			select {
			case <-ctx.Done():
				return
			case ch <- minio.ObjectInfo{Key: s.objectKey(o.Key)}:
			}
		}
	}()

	var firstErr error
	for e := range s.client.RemoveObjects(ctx, s.bucket, ch, minio.RemoveObjectsOptions{}) {
		if e.Err != nil && firstErr == nil {
			firstErr = fmt.Errorf("удаление %s: %w", e.ObjectName, e.Err)
		}
	}
	if firstErr != nil {
		return 0, firstErr
	}
	return len(objects), nil
}

func (s *s3Backend) lockProtected(key string) bool {
	return strings.HasPrefix(strings.TrimLeft(key, "/"), repoDataPrefix())
}

// deleteVersions never issues an unversioned DELETE in an Object Lock bucket.
// This applies to staging and health probes too: they are not retained, but an
// unversioned delete would only hide their data behind a delete marker and
// leak the old version forever. Retained repository objects are checked before
// removal and Governance bypass is never requested.
//
// In an Object Lock
// bucket that operation can succeed by creating a delete marker, hiding a
// retained backup from ordinary reads even though the protected version still
// exists.
func (s *s3Backend) deleteVersions(ctx context.Context, key string, exact bool) (int, error) {
	full := s.objectKey(key)
	var versions []minio.ObjectInfo
	unique := map[string]bool{}
	for object := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{
		Prefix: full, Recursive: true, WithVersions: true,
	}) {
		if object.Err != nil {
			return 0, fmt.Errorf("перечисление версий %s: %w", key, object.Err)
		}
		if exact && object.Key != full {
			continue
		}
		versions = append(versions, object)
		if !object.IsDeleteMarker {
			unique[object.Key] = true
		}
	}
	if len(versions) == 0 {
		return 0, nil
	}

	now := time.Now().UTC()
	for _, object := range versions {
		if object.IsDeleteMarker {
			continue
		}
		relative := object.Key
		if s.prefix != "" {
			relative = strings.TrimPrefix(relative, s.prefix+"/")
		}
		if !s.lockProtected(relative) {
			continue
		}
		_, until, err := s.client.GetObjectRetention(ctx, s.bucket, object.Key, object.VersionID)
		if err != nil {
			return 0, fmt.Errorf("проверка retention %s: %w", key, err)
		}
		if until != nil && until.After(now) {
			return 0, fmt.Errorf("объект %s заблокирован Object Lock до %s",
				key, until.UTC().Format(time.RFC3339))
		}
	}
	for _, object := range versions {
		if err := s.client.RemoveObject(ctx, s.bucket, object.Key, minio.RemoveObjectOptions{
			VersionID: object.VersionID,
		}); err != nil {
			return 0, fmt.Errorf("удаление версии %s: %w", key, err)
		}
	}
	return len(unique), nil
}

func (s *s3Backend) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	var out []ObjectInfo
	full := s.objectKey(prefix)
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{
		Prefix:    full,
		Recursive: true,
	}) {
		if obj.Err != nil {
			return nil, fmt.Errorf("перечисление %s: %w", prefix, obj.Err)
		}
		key := obj.Key
		if s.prefix != "" {
			key = strings.TrimPrefix(key, s.prefix+"/")
		}
		out = append(out, ObjectInfo{
			Key:      key,
			Size:     obj.Size,
			Modified: obj.LastModified.UTC(),
			ETag:     obj.ETag,
		})
	}
	return out, nil
}

// Usage is not answerable for object storage: S3 has no notion of free space.
// Reporting zeros tells the caller "unknown" rather than "full".
func (s *s3Backend) Usage(ctx context.Context) (int64, int64, error) {
	var used int64
	objects, err := s.List(ctx, "")
	if err != nil {
		return 0, 0, nil
	}
	for _, o := range objects {
		used += o.Size
	}
	return 0, used, nil
}

func (s *s3Backend) Check(ctx context.Context) error {
	ok, err := s.client.BucketExists(ctx, s.bucket)
	if err != nil {
		return fmt.Errorf("проверка bucket %s: %w", s.bucket, err)
	}
	if !ok {
		return fmt.Errorf("bucket %q не существует или недоступен под этими ключами", s.bucket)
	}
	if err := s.CheckObjectLock(ctx); err != nil {
		return err
	}
	return runCheck(ctx, s)
}

func translateS3Error(key string, err error) error {
	if err == nil {
		return nil
	}
	resp := minio.ToErrorResponse(err)
	switch resp.Code {
	case "NoSuchKey", "NoSuchBucket", "NotFound":
		return fmt.Errorf("%w: %s", ErrNotExist, key)
	}
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: %s", ErrNotExist, key)
	}
	return err
}
