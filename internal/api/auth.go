package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"github.com/bingdilotus/chronotope/internal/store"
)

// bearerToken 提取 Authorization: Bearer <key>。
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
}

type ctxKey string

const orgKey ctxKey = "auth.org"

// AuthOrg 取认证后的 org（off 模式为 ""——匿名，org 从路径取）。
// sha256Hex 哈希 key（认证查找 + 存储）。
func sha256Hex(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// newAPIKey 生成 ck_ 前缀 key（32 字节随机 → hex）。
func newAPIKey() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "ck_" + hex.EncodeToString(buf), nil
}

// authMiddleware 多租户认证（API_AUTH_MODE）：
//   - off（默认）：无 key 请求匿名放行（org 从路径取；dev/e2e 兼容）
//   - on：强制 Bearer key → 查 org 注入 context；缺失/未知 401
//
// 归属校验（on 模式）：/orgs/{orgID}、/agents/{agentID}、/sessions/{sessionID}
// 的资源 org 必须等于 key 的 org，否则 403；/webhooks 与静态端点不校验。
func (h *Handler) AuthMiddleware(mode, adminKey string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			org := ""
			principal := ""
			admin := false
			key := bearerToken(r)
			if key != "" {
				if adminKey != "" && key == adminKey {
					admin = true // 管理面引导 key：全放行（org 从路径取）
				} else {
					row, err := h.Store.GetAPIKeyByHash(r.Context(), sha256Hex(key))
					if err != nil {
						if errors.Is(err, store.ErrNotFound) {
							writeError(w, http.StatusUnauthorized, 401, "invalid api key")
						} else {
							writeError(w, http.StatusInternalServerError, 500, err.Error())
						}
						return
					}
					org = row.OrgID
					principal = row.UserID // 期 3 §A：key 绑定的技术主体
				}
			}
			ctx := context.WithValue(r.Context(), orgKey, org)
			ctx = context.WithValue(ctx, userKey, principal)
			if mode == "on" {
				// 匿名路径（healthz/webhooks）与 admin key 在 on 模式下放行
				if !anonymousPath(r) && !admin && org == "" {
					writeError(w, http.StatusUnauthorized, 401, "missing api key（API_AUTH_MODE=on）")
					return
				}
				if !admin && org != "" && !resourceOwnedBy(r, org) {
					writeError(w, http.StatusForbidden, 403, "org 归属不符")
					return
				}
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// userKey 是 principal 的上下文键（期 3 §A）。
type userCtxKey struct{}

var userKey = userCtxKey{}

// PrincipalFrom 取上下文中的技术主体（空 = 租户级调用）。
func PrincipalFrom(ctx context.Context) string {
	if v, ok := ctx.Value(userKey).(string); ok {
		return v
	}
	return ""
}

// anonymousPath 匿名路径：无需 key（webhook 回调方/健康检查）。
func anonymousPath(r *http.Request) bool {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) == 0 {
		return false
	}
	return parts[0] == "healthz" || parts[0] == "webhooks"
}

// resourceOwnedBy 校验路径资源归属（on 模式；匿名路径直接放行）。
func resourceOwnedBy(r *http.Request, org string) bool {
	path := strings.Trim(r.URL.Path, "/")
	parts := strings.Split(path, "/")
	if len(parts) == 0 {
		return true
	}
	switch parts[0] {
	case "orgs":
		return len(parts) < 2 || parts[1] == org
	case "webhooks", "healthz":
		return true
	default:
		return true // agents/sessions 的资源归属由 handler 内查（需 store 联查，中间件不重复查询）
	}
}
