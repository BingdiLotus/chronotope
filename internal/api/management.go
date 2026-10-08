package api

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/bingdilotus/chronotope/internal/store"
)

// 期 5 §A：多租户管理面——成员/用量聚合/配额/账单导出。
// 认证：沿用现有 adminAuth 中间件（API_ADMIN_KEY）；MVP 无角色门控
// （角色模型的执行面是审批策略——管理 API 本身走 admin key）。

// registerManagementRoutes 挂管理面路由（期 5 §A）——顶层完整路径
// （chi 同模式 Route 块重复注册 panic——并入现有块或顶层直挂）。
func registerManagementRoutes(r chi.Router, h *Handler) {
	r.Get("/orgs/{orgID}/members", h.listMembers)
	r.Post("/orgs/{orgID}/members", h.addMember)
	r.Delete("/orgs/{orgID}/members/{userID}", h.removeMember)
	r.Get("/orgs/{orgID}/usage", h.orgUsage)
	r.Get("/orgs/{orgID}/quota", h.getOrgQuota)
	r.Put("/orgs/{orgID}/quota", h.updateOrgQuota)
	r.Get("/orgs/{orgID}/billing/export", h.billingExport)
}

// GET /orgs/{orgID}/members —— 列成员（角色排序）。
func (h *Handler) listMembers(w http.ResponseWriter, r *http.Request) {
	orgID := chi.URLParam(r, "orgID")
	members, err := h.Store.ListMembers(r.Context(), orgID)
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"members": members})
}

// POST /orgs/{orgID}/members —— 加成员（body: {user_id, role}；直建无邮件流）。
func (h *Handler) addMember(w http.ResponseWriter, r *http.Request) {
	orgID := chi.URLParam(r, "orgID")
	var req struct {
		UserID string `json:"user_id"`
		Role   string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.UserID == "" || !store.ValidRole(req.Role) {
		writeJSON(w, 400, map[string]any{"error": "需要 user_id 与合法 role（org_admin|member|auditor）"})
		return
	}
	if err := h.Store.AddMember(r.Context(), store.Member{OrgID: orgID, UserID: req.UserID, Role: req.Role}); err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 201, map[string]any{"org_id": orgID, "user_id": req.UserID, "role": req.Role})
}

// DELETE /orgs/{orgID}/members/{userID} —— 移除成员。
func (h *Handler) removeMember(w http.ResponseWriter, r *http.Request) {
	orgID := chi.URLParam(r, "orgID")
	userID := chi.URLParam(r, "userID")
	if err := h.Store.RemoveMember(r.Context(), orgID, userID); err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"removed": userID})
}

// GET /orgs/{orgID}/usage?granularity=hour|day|month —— 用量聚合。
func (h *Handler) orgUsage(w http.ResponseWriter, r *http.Request) {
	orgID := chi.URLParam(r, "orgID")
	granularity := r.URL.Query().Get("granularity")
	if granularity == "" {
		granularity = "day"
	}
	rows, err := h.Store.AggregateOrgUsage(r.Context(), orgID, granularity)
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"granularity": granularity, "usage": rows})
}

// GET /orgs/{orgID}/quota —— 读配额（orgs.quotas 原样 + 预算键展开）。
func (h *Handler) getOrgQuota(w http.ResponseWriter, r *http.Request) {
	orgID := chi.URLParam(r, "orgID")
	org, err := h.Store.GetOrg(r.Context(), orgID)
	if err != nil {
		writeJSON(w, 404, map[string]any{"error": "org not found"})
		return
	}
	writeJSON(w, 200, map[string]any{"org_id": orgID, "quotas": org.Quotas})
}

// PUT /orgs/{orgID}/quota —— 写配额（合并语义；键 daily_token_budget/
// daily_compute_seconds——0/缺省 = 无限）。
func (h *Handler) updateOrgQuota(w http.ResponseWriter, r *http.Request) {
	orgID := chi.URLParam(r, "orgID")
	var quotas map[string]any
	if err := json.NewDecoder(r.Body).Decode(&quotas); err != nil || len(quotas) == 0 {
		writeJSON(w, 400, map[string]any{"error": "需要非空配额对象"})
		return
	}
	if err := h.Store.UpdateOrgQuotas(r.Context(), orgID, quotas); err != nil {
		status := 500
		if err == store.ErrNotFound {
			status = 404
		}
		writeJSON(w, status, map[string]any{"error": err.Error()})
		return
	}
	h.getOrgQuota(w, r)
}

// GET /orgs/{orgID}/billing/export —— 账单 CSV（事件级用量导出——期 5 §A
// 边界：订阅/支付对接后置，账单导出是边界）。
func (h *Handler) billingExport(w http.ResponseWriter, r *http.Request) {
	orgID := chi.URLParam(r, "orgID")
	rows, err := h.Store.AggregateOrgUsage(r.Context(), orgID, "hour")
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="billing-%s-%d.csv"`, orgID, time.Now().Unix()))
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"bucket", "active_seconds", "tokens_in", "tokens_out", "compute_seconds"})
	for _, row := range rows {
		_ = cw.Write([]string{
			row.Bucket.UTC().Format(time.RFC3339),
			strconv.FormatFloat(row.ActiveSeconds, 'f', 2, 64),
			strconv.FormatInt(row.TokensIn, 10),
			strconv.FormatInt(row.TokensOut, 10),
			strconv.FormatFloat(row.ComputeSeconds, 'f', 2, 64),
		})
	}
	cw.Flush()
}
