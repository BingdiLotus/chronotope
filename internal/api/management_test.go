package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// TestManagementMembers 期 5 §A：成员 CRUD + 角色校验。
func TestManagementMembers(t *testing.T) {
	h, _, _ := setup(t)
	do := func(method, path, body string) (int, map[string]any) {
		rec := doJSON(t, h.Router(), method, path, body, nil)
		var m map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
		if m == nil {
			m = map[string]any{}
		}
		return rec.Code, m
	}

	code, m := do(http.MethodPost, "/orgs/org_m1/members", `{"user_id":"u1","role":"org_admin"}`)
	if code != 201 || m["role"] != "org_admin" {
		t.Fatalf("add member: %d %+v", code, m)
	}
	code, _ = do(http.MethodPost, "/orgs/org_m1/members", `{"user_id":"u2","role":"member"}`)
	if code != 201 {
		t.Fatalf("add member2: %d", code)
	}
	code, _ = do(http.MethodPost, "/orgs/org_m1/members", `{"user_id":"u3","role":"hacker"}`)
	if code != 400 {
		t.Fatalf("非法角色应 400: %d", code)
	}
	code, m = do(http.MethodGet, "/orgs/org_m1/members", "")
	if code != 200 {
		t.Fatalf("list members: %d", code)
	}
	if len(m["members"].([]any)) != 2 {
		t.Fatalf("成员数应 2: %+v", m["members"])
	}
	code, _ = do(http.MethodDelete, "/orgs/org_m1/members/u1", "")
	if code != 200 {
		t.Fatalf("remove member: %d", code)
	}
}

// TestManagementUsageAndBilling 期 5 §A：用量聚合 + 账单 CSV。
func TestManagementUsageAndBilling(t *testing.T) {
	h, _, _ := setup(t)
	rec := doJSON(t, h.Router(), http.MethodGet, "/orgs/org_m2/usage?granularity=day", "", nil)
	if rec.Code != 200 {
		t.Fatalf("usage: %d %s", rec.Code, rec.Body.String())
	}
	rec = doJSON(t, h.Router(), http.MethodGet, "/orgs/org_m2/billing/export", "", nil)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "text/csv" {
		t.Fatalf("billing: %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	if len(rec.Body.String()) < 40 {
		t.Fatalf("CSV 内容过短: %q", rec.Body.String())
	}
}

// TestManagementQuota 期 5 §A：配额读改。
func TestManagementQuota(t *testing.T) {
	h, fs, _ := setup(t)
	_ = fs.CreateOrg(context.Background(), "org_m3", "m3")
	rec := doJSON(t, h.Router(), http.MethodPut, "/orgs/org_m3/quota", `{"daily_token_budget":5000}`, nil)
	if rec.Code != 200 {
		t.Fatalf("put quota: %d", rec.Code)
	}
	rec = doJSON(t, h.Router(), http.MethodGet, "/orgs/org_m3/quota", "", nil)
	if rec.Code != 200 {
		t.Fatalf("get quota: %d", rec.Code)
	}
}
