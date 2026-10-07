package execproto

import (
	"context"
	"os"
	"testing"

	"github.com/minio/minio-go/v7"
)

// TestBlobStorePutIdempotent 期 2 §A：内容寻址幂等——同内容两次 PUT 只存一个
// 对象（跨会话 dedup 前提）。env-gated：RUSTFS_TEST_ENDPOINT 未设时跳过
// （本地 CI 单测阶段无 RustFS；e2e 覆盖真链路）。
func TestBlobStorePutIdempotent(t *testing.T) {
	endpoint := os.Getenv("RUSTFS_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("RUSTFS_TEST_ENDPOINT 未设置（本地 CI 单测阶段无 RustFS；e2e 覆盖）")
	}
	ctx := context.Background()
	b, err := NewBlobStore(endpoint, os.Getenv("RUSTFS_TEST_ACCESS_KEY"), os.Getenv("RUSTFS_TEST_SECRET_KEY"), "workspaces-test", false)
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}
	if err := b.EnsureBucket(ctx); err != nil {
		t.Fatalf("bucket: %v", err)
	}
	f, err := os.CreateTemp("", "blob-test-*")
	if err != nil {
		t.Fatalf("temp: %v", err)
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString("同一内容"); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.Close()
	h1, size1, err := b.PutFile(ctx, "org-test", "s1", f.Name())
	if err != nil {
		t.Fatalf("put1: %v", err)
	}
	h2, size2, err := b.PutFile(ctx, "org-test", "s2", f.Name()) // 不同会话同内容
	if err != nil {
		t.Fatalf("put2: %v", err)
	}
	if h1 != h2 || size1 != size2 {
		t.Fatalf("内容寻址应同 hash: %s vs %s", h1, h2)
	}
	// 对象数 = 1（同 hash 跳过第二次上传——dedup）
	count := 0
	for obj := range b.client.ListObjects(ctx, b.bucket, minio.ListObjectsOptions{Prefix: "org-test/blobs/", Recursive: true}) {
		if obj.Err != nil {
			t.Fatalf("list: %v", obj.Err)
		}
		count++
	}
	if count != 1 {
		t.Fatalf("同 hash 应只存一个对象: %d", count)
	}
	// GetFile 恢复写回
	dst := f.Name() + ".restored"
	defer os.Remove(dst)
	if err := b.GetFile(ctx, "org-test", "s2", h2, dst); err != nil {
		t.Fatalf("get: %v", err)
	}
	data, _ := os.ReadFile(dst)
	if string(data) != "同一内容" {
		t.Fatalf("恢复内容不符: %q", data)
	}
}
