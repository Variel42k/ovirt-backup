package repo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"adveng/jh_virt/internal/model"
)

// s3Backend stores backups in S3-compatible object storage: AWS S3, MinIO,
// Ceph RGW and the Russian clouds that speak the same protocol.
type s3Backend struct {
	name         string
	client       *minio.Client
	bucket       string
	prefix       string
	storageClass string
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
	opts := minio.PutObjectOptions{ContentType: "application/octet-stream"}
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
	err := s.client.RemoveObject(ctx, s.bucket, s.objectKey(key), minio.RemoveObjectOptions{})
	if err != nil && !errors.Is(translateS3Error(key, err), ErrNotExist) {
		return err
	}
	return nil
}

func (s *s3Backend) DeletePrefix(ctx context.Context, prefix string) (int, error) {
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
