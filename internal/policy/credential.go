package policy

import (
	"context"
	"os"
)

// ResolvedCredential 是解析结果（dsh 同款：值与来源层——来源层供审计）。
type ResolvedCredential struct {
	Value  string `json:"value"`
	Source string `json:"source"` // env | org | agent | file
}

// CredentialInfo 是 UI 安全的描述（describe——值无槽；configured/source/
// writable 三事实）。
type CredentialInfo struct {
	Configured bool   `json:"configured"`
	Source     string `json:"source,omitempty"`
	Writable   bool   `json:"writable"`
}

// CredentialResolver 是凭据解析缝（凭据服务设计 §层 1）：引用名 →
// 值——每次操作重解析（轮换即时生效）。
type CredentialResolver interface {
	Resolve(ctx context.Context, orgID, ref string) (*ResolvedCredential, error)
	Describe(ctx context.Context, orgID, ref string) CredentialInfo
}

// EnvResolver 是参考实现（阶段 ① env 后端先行——引用名即环境变量名；
// 零行为变化 fallback 到现有 .env 注入）。
type EnvResolver struct{}

// Resolve 从环境解析（org 前缀 > 裸名——引用名 = env 名）。
func (EnvResolver) Resolve(_ context.Context, orgID, ref string) (*ResolvedCredential, error) {
	if v := os.Getenv(ref); v != "" {
		return &ResolvedCredential{Value: v, Source: "env"}, nil
	}
	if orgID != "" {
		if v := os.Getenv(orgID + "_" + ref); v != "" {
			return &ResolvedCredential{Value: v, Source: "org"}, nil
		}
	}
	return nil, nil // 未配置——调用方显式处理（不静默换凭据）
}

// Describe UI 安全描述（值永不出现在视图）。
func (EnvResolver) Describe(_ context.Context, orgID, ref string) CredentialInfo {
	if v := os.Getenv(ref); v != "" {
		return CredentialInfo{Configured: true, Source: "env", Writable: true}
	}
	if orgID != "" && os.Getenv(orgID+"_"+ref) != "" {
		return CredentialInfo{Configured: true, Source: "org", Writable: true}
	}
	return CredentialInfo{Configured: false}
}
