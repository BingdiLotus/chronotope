"use client";

// 期 5 §B：org 管理页——用量看板（三轴 + 配额进度）、成员列表、审计流。
// 数据源：§A 管理面 API（/usage /quota /members /audit）。
import { useCallback, useEffect, useState } from "react";
import { useParams } from "next/navigation";

const API = process.env.NEXT_PUBLIC_API_URL || "http://localhost:8080";

type UsageRow = {
  bucket: string;
  active_seconds: number;
  tokens_in: number;
  tokens_out: number;
  compute_seconds: number;
};
type Member = { org_id: string; user_id: string; role: string; created_at: string };
type AuditEvent = { id: string; kind: string; at: string; payload?: Record<string, unknown> };

export default function OrgPage() {
  const params = useParams<{ id: string }>();
  const org = decodeURIComponent(params.id || "");
  const [usage, setUsage] = useState<UsageRow[]>([]);
  const [quotas, setQuotas] = useState<Record<string, unknown>>({});
  const [members, setMembers] = useState<Member[]>([]);
  const [audit, setAudit] = useState<AuditEvent[]>([]);
  const [error, setError] = useState("");
  const [newMember, setNewMember] = useState({ user_id: "", role: "member" });
  const [quotaInput, setQuotaInput] = useState({ daily_token_budget: "", daily_compute_seconds: "" });

  const refresh = useCallback(() => {
    if (!org) return;
    fetch(`${API}/orgs/${org}/usage?granularity=day`)
      .then((r) => r.json())
      .then((d) => setUsage(d.usage || []))
      .catch((e) => setError(String(e)));
    fetch(`${API}/orgs/${org}/quota`)
      .then((r) => r.json())
      .then((d) => setQuotas(d.quotas || {}))
      .catch(() => {});
    fetch(`${API}/orgs/${org}/members`)
      .then((r) => r.json())
      .then((d) => setMembers(d.members || []))
      .catch(() => {});
    fetch(`${API}/orgs/${org}/audit?kind=approval`)
      .then((r) => r.json())
      .then((d) => setAudit(d.events || []))
      .catch(() => {});
  }, [org]);

  useEffect(() => {
    refresh();
    const t = setInterval(refresh, 15000);
    return () => clearInterval(t);
  }, [refresh]);

  const totalTokens = usage.reduce((s, u) => s + u.tokens_in + u.tokens_out, 0);
  const totalCompute = usage.reduce((s, u) => s + u.compute_seconds, 0);
  const budget = Number(quotas.daily_token_budget || 0);
  const quotaPct = budget > 0 ? Math.min(100, Math.round((totalTokens / budget) * 100)) : 0;

  const addMember = () => {
    if (!newMember.user_id) return;
    fetch(`${API}/orgs/${org}/members`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify(newMember),
    })
      .then((r) => (r.ok ? refresh() : r.json().then((d) => setError(d.error || "成员添加失败"))))
      .catch((e) => setError(String(e)));
  };
  const removeMember = (userID: string) => {
    fetch(`${API}/orgs/${org}/members/${userID}`, { method: "DELETE" }).then((r) => r.ok && refresh());
  };
  const saveQuota = () => {
    const body: Record<string, number> = {};
    if (quotaInput.daily_token_budget) body.daily_token_budget = Number(quotaInput.daily_token_budget);
    if (quotaInput.daily_compute_seconds) body.daily_compute_seconds = Number(quotaInput.daily_compute_seconds);
    fetch(`${API}/orgs/${org}/quota`, {
      method: "PUT",
      headers: { "content-type": "application/json" },
      body: JSON.stringify(body),
    }).then((r) => (r.ok ? refresh() : setError("配额保存失败")));
  };

  return (
    <main style={{ fontFamily: "system-ui, sans-serif", padding: "2rem", maxWidth: 860, margin: "0 auto" }}>
      <h1>
        组织管理：<code>{org}</code>
      </h1>
      <p style={{ color: "#666" }}>
        <a href="/">← 会话列表</a>
      </p>
      {error && <p style={{ color: "red" }}>{error}</p>}

      <section style={{ border: "1px solid #ddd", borderRadius: 8, padding: 16, marginBottom: 16 }}>
        <h2>用量看板（日粒度）</h2>
        <p>
          累计 token：<b>{totalTokens}</b> · 计算秒：<b>{totalCompute.toFixed(1)}</b>
        </p>
        {(
          <div>
            <span>配额进度（token）：{quotaPct}%</span>
            <div style={{ background: "#eee", height: 10, borderRadius: 5, margin: "6px 0" }}>
              <div
                data-testid="quota-bar"
                style={{ width: `${quotaPct}%`, background: quotaPct > 80 ? "#e05555" : "#3b82f6", height: 10, borderRadius: 5 }}
              />
            </div>
          </div>
        )}
        <table style={{ width: "100%", borderCollapse: "collapse", fontSize: 14 }}>
          <thead>
            <tr>
              <th style={{ textAlign: "left", borderBottom: "1px solid #ddd", padding: 4 }}>bucket</th>
              <th style={{ textAlign: "right", borderBottom: "1px solid #ddd", padding: 4 }}>tokens_in</th>
              <th style={{ textAlign: "right", borderBottom: "1px solid #ddd", padding: 4 }}>tokens_out</th>
              <th style={{ textAlign: "right", borderBottom: "1px solid #ddd", padding: 4 }}>compute_s</th>
            </tr>
          </thead>
          <tbody data-testid="usage-rows">
            {usage.map((u) => (
              <tr key={u.bucket}>
                <td style={{ padding: 4 }}>{new Date(u.bucket).toLocaleDateString()}</td>
                <td style={{ textAlign: "right", padding: 4 }}>{u.tokens_in}</td>
                <td style={{ textAlign: "right", padding: 4 }}>{u.tokens_out}</td>
                <td style={{ textAlign: "right", padding: 4 }}>{u.compute_seconds.toFixed(1)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </section>

      <section style={{ border: "1px solid #ddd", borderRadius: 8, padding: 16, marginBottom: 16 }}>
        <h2>配额设置</h2>
        <label>
          daily_token_budget：
          <input
            data-testid="quota-token"
            value={quotaInput.daily_token_budget}
            onChange={(e) => setQuotaInput({ ...quotaInput, daily_token_budget: e.target.value })}
            style={{ margin: 4, padding: 4, width: 120 }}
          />
        </label>
        <label>
          daily_compute_seconds：
          <input
            value={quotaInput.daily_compute_seconds}
            onChange={(e) => setQuotaInput({ ...quotaInput, daily_compute_seconds: e.target.value })}
            style={{ margin: 4, padding: 4, width: 120 }}
          />
        </label>
        <button data-testid="quota-save" onClick={saveQuota} style={{ marginLeft: 8 }}>
          保存
        </button>
        <p style={{ color: "#888", fontSize: 13 }}>当前：{JSON.stringify(quotas)}</p>
      </section>

      <section style={{ border: "1px solid #ddd", borderRadius: 8, padding: 16, marginBottom: 16 }}>
        <h2>成员</h2>
        <div>
          <input
            data-testid="member-id"
            placeholder="user_id"
            value={newMember.user_id}
            onChange={(e) => setNewMember({ ...newMember, user_id: e.target.value })}
            style={{ padding: 4, width: 200 }}
          />
          <select
            data-testid="member-role"
            value={newMember.role}
            onChange={(e) => setNewMember({ ...newMember, role: e.target.value })}
            style={{ margin: "0 8px", padding: 4 }}
          >
            <option value="org_admin">org_admin</option>
            <option value="member">member</option>
            <option value="auditor">auditor</option>
          </select>
          <button data-testid="member-add" onClick={addMember}>
            添加成员
          </button>
        </div>
        <ul data-testid="member-list">
          {members.map((m) => (
            <li key={m.user_id} style={{ margin: "6px 0" }}>
              <b>{m.user_id}</b> <span style={{ color: "#666" }}>（{m.role}）</span>
              <button onClick={() => removeMember(m.user_id)} style={{ marginLeft: 12, color: "#c00" }}>
                移除
              </button>
            </li>
          ))}
        </ul>
      </section>

      <section style={{ border: "1px solid #ddd", borderRadius: 8, padding: 16 }}>
        <h2>审批审计流（只读）</h2>
        <ul data-testid="audit-list">
          {audit.map((a) => (
            <li key={a.id || a.at} style={{ margin: "6px 0", fontSize: 13 }}>
              <b>{a.kind}</b> <span style={{ color: "#888" }}>{a.at ? new Date(a.at).toLocaleString() : ""}</span>
              {a.payload && <span style={{ color: "#999" }}> {JSON.stringify(a.payload).slice(0, 120)}</span>}
            </li>
          ))}
        </ul>
        {audit.length === 0 && <p style={{ color: "#999" }}>暂无审批审计事件。</p>}
      </section>
    </main>
  );
}
