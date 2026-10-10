package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/bingdilotus/chronotope/internal/core/session"
	"time"

	"github.com/jackc/pgx/v5"
)

// Org 是 orgs 行（quotas 为原样 map；预算键见 OrgQuotaKeys）。
// Org（期 7 下沉：core/session）。
type Org = session.Org

func isNoRowsErr(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}

// OrgQuotaKeys 是 orgs.quotas 中的预算键（三级熔断 ② 级；0/缺省 = 无限）。
const (
	QuotaDailyTokenBudget   = "daily_token_budget"
	QuotaDailyComputeBudget = "daily_compute_seconds"
)

// GetOrg 读 org（含 quotas 原样 map）。
func (s *Store) GetOrg(ctx context.Context, id string) (*Org, error) {
	const q = `SELECT id, name, quotas FROM orgs WHERE id = $1`
	var o Org
	var quotasJSON json.RawMessage
	if err := s.Pool.QueryRow(ctx, q, id).Scan(&o.ID, &o.Name, &quotasJSON); err != nil {
		if isNoRowsErr(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("store: get org: %w", err)
	}
	_ = json.Unmarshal(quotasJSON, &o.Quotas)
	if o.Quotas == nil {
		o.Quotas = map[string]any{}
	}
	return &o, nil
}

// UpdateOrgQuotas 合并更新 org 配额（预算键；其余键保留）。
func (s *Store) UpdateOrgQuotas(ctx context.Context, id string, quotas map[string]any) error {
	const q = `UPDATE orgs SET quotas = quotas || $2::jsonb WHERE id = $1`
	b, err := json.Marshal(quotas)
	if err != nil {
		return fmt.Errorf("store: marshal quotas: %w", err)
	}
	tag, err := s.Pool.Exec(ctx, q, id, b)
	if err != nil {
		return fmt.Errorf("store: update org quotas: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// OrgDailyUsage 聚合 org 当日用量（token 总数 + 计算秒；usage 为 1min 桶，
// 由 api 聚合器维护，worker 校验时最多滞后一个聚合周期）。
func (s *Store) OrgDailyUsage(ctx context.Context, orgID string, day time.Time) (tokens int64, computeSeconds float64, err error) {
	const q = `
SELECT COALESCE(SUM(u.tokens_in + u.tokens_out), 0), COALESCE(SUM(u.compute_seconds), 0)
FROM usage u JOIN sessions sess ON sess.id = u.session_id
WHERE sess.org_id = $1 AND u.bucket >= $2`
	if err := s.Pool.QueryRow(ctx, q, orgID, day).Scan(&tokens, &computeSeconds); err != nil {
		return 0, 0, fmt.Errorf("store: org daily usage: %w", err)
	}
	return tokens, computeSeconds, nil
}

// GetOrgQuotas 读租户配额（期 3 §A 策略缝参考实现的数据装配）。
func (s *Store) GetOrgQuotas(ctx context.Context, orgID string) (map[string]any, error) {
	org, err := s.GetOrg(ctx, orgID)
	if err != nil || org == nil {
		return nil, err
	}
	return org.Quotas, nil
}
