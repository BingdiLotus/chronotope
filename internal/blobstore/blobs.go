package blobstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// BlobStore 是工作区 blob 合同的 S3 门面（期 2 §A：内容寻址对象图 + 目录索引；
// 对象存 RustFS bucket workspaces/{org}/{session}/blobs/{hash}）。写入幂等：
// 同 hash 对象已存在则跳过（跨会话/跨沙箱 dedup 的前提）。
type BlobStore struct {
	client *minio.Client
	bucket string
}

// NewBlobStore 构造（endpoint 形如 localhost:9000；keys 为空时用 RustFS 默认凭证）。
func NewBlobStore(endpoint, accessKey, secretKey, bucket string, secure bool) (*BlobStore, error) {
	if bucket == "" {
		bucket = "workspaces"
	}
	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: secure,
	})
	if err != nil {
		return nil, fmt.Errorf("blob: minio client: %w", err)
	}
	return &BlobStore{client: client, bucket: bucket}, nil
}

// ObjectKey 计算对象键（org/blobs/{sha256}）——内容寻址在 org 维度 dedup
// （同 hash 跨会话共享对象；session 目录索引在 workspace_files 表）。
func ObjectKey(orgID, sessionID, hash string) string {
	return strings.Join([]string{orgID, "blobs", hash}, "/")
}

// PutFile 读本地文件 → sha256 → 幂等 PUT（已存在同 hash 对象则跳过）。
// 返回 (hash, size, error)。
func (b *BlobStore) PutFile(ctx context.Context, orgID, sessionID, localPath string) (string, int64, error) {
	data, err := os.ReadFile(localPath)
	if err != nil {
		return "", 0, fmt.Errorf("blob: read local: %w", err)
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	key := ObjectKey(orgID, sessionID, hash)
	// 内容寻址幂等：同 hash 对象已存在 → 跳过（StatObject 探测）
	if _, err := b.client.StatObject(ctx, b.bucket, key, minio.StatObjectOptions{}); err == nil {
		return hash, int64(len(data)), nil
	}
	_, err = b.client.PutObject(ctx, b.bucket, key, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{
		ContentType: "application/octet-stream",
	})
	if err != nil {
		return "", 0, fmt.Errorf("blob: put object: %w", err)
	}
	return hash, int64(len(data)), nil
}

// GetFile 拉取对象写回本地路径（恢复）。
func (b *BlobStore) GetFile(ctx context.Context, orgID, sessionID, hash, localPath string) error {
	obj, err := b.client.GetObject(ctx, b.bucket, ObjectKey(orgID, "", hash), minio.GetObjectOptions{})
	if err != nil {
		return fmt.Errorf("blob: get object: %w", err)
	}
	defer obj.Close()
	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		return fmt.Errorf("blob: mkdir: %w", err)
	}
	f, err := os.Create(localPath)
	if err != nil {
		return fmt.Errorf("blob: create local: %w", err)
	}
	defer f.Close()
	if _, err := io.Copy(f, obj); err != nil {
		return fmt.Errorf("blob: copy: %w", err)
	}
	return nil
}

// EnsureBucket 幂等建桶。
func (b *BlobStore) EnsureBucket(ctx context.Context) error {
	exists, err := b.client.BucketExists(ctx, b.bucket)
	if err != nil {
		return fmt.Errorf("blob: bucket exists: %w", err)
	}
	if !exists {
		if err := b.client.MakeBucket(ctx, b.bucket, minio.MakeBucketOptions{}); err != nil {
			return fmt.Errorf("blob: make bucket: %w", err)
		}
	}
	return nil
}

// SyncWindow 是写路径的同步重试窗口（失败不阻断 run——标记 syncing 后重试）。

// PutBytes 上传内存字节到指定对象键（归档等非内容寻址用途）。
func (b *BlobStore) PutBytes(ctx context.Context, key string, data []byte) error {
	if _, err := b.client.PutObject(ctx, b.bucket, key, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{
		ContentType: "application/gzip",
	}); err != nil {
		return fmt.Errorf("blob: put bytes: %w", err)
	}
	return nil
}
