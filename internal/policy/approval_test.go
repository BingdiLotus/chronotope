package policy

import (
	"context"
	"errors"
	"testing"
)

// TestOrgApprovalRoute 模式匹配路由：命中 pattern 返回集合+TTL、未命中回落
// ManualOnly 语义、空 patterns 全匹配——期 3 §B 参考实现的验收。
func TestOrgApprovalRoute(t *testing.T) {
	p := OrgApprovalPolicy{
		Get: func(context.Context, string) ([]string, []string, int64, error) {
			return []string{"bash", "deploy"}, []string{"ops", "admin"}, 3600, nil
		},
	}
	// 命中
	app, ttl, err := p.Route(context.Background(), ApprovalRequest{TenantID: "o", Tool: "bash"})
	if err != nil || len(app) != 2 || ttl != 3600 {
		t.Fatalf("命中路由: %v %d err=%v", app, ttl, err)
	}
	// 未命中 → 回落（无集合 = ManualOnly 人工）
	app, _, err = p.Route(context.Background(), ApprovalRequest{TenantID: "o", Tool: "read_file"})
	if err != nil || len(app) != 0 {
		t.Fatalf("未命中应回落: %v err=%v", app, err)
	}
	// 空 patterns = 全匹配
	p2 := OrgApprovalPolicy{
		Get: func(context.Context, string) ([]string, []string, int64, error) {
			return nil, []string{"ops"}, 60, nil
		},
	}
	app, ttl, err = p2.Route(context.Background(), ApprovalRequest{TenantID: "o", Tool: "anything"})
	if err != nil || len(app) != 1 || ttl != 60 {
		t.Fatalf("空 patterns 全匹配: %v %d err=%v", app, ttl, err)
	}
	// Get 失败透传（路由错误显式——不静默回落）
	p3 := OrgApprovalPolicy{
		Get: func(context.Context, string) ([]string, []string, int64, error) {
			return nil, nil, 0, errors.New("db down")
		},
	}
	if _, _, err := p3.Route(context.Background(), ApprovalRequest{TenantID: "o", Tool: "bash"}); err == nil {
		t.Fatal("Get 失败应透传")
	}
}

// TestManualOnly 默认实现的空集合语义（控制台人工）。
func TestManualOnly(t *testing.T) {
	app, ttl, err := ManualOnly{}.Route(context.Background(), ApprovalRequest{})
	if err != nil || len(app) != 0 || ttl != 0 {
		t.Fatalf("ManualOnly: %v %d err=%v", app, ttl, err)
	}
}
