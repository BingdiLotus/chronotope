"use client";

// 会话页（W4 控制台 + P2-2 人控闭环）：时间轴回放（SSE after=seq）+ 人控操作
// （class 2 审批批准/拒绝、欠费冻结解冻）+ 分层记忆面板 + 三轴计量。
import { useEffect, useMemo, useRef, useState } from "react";
import { useParams } from "next/navigation";

type TimelineEvent = {
  seq: number;
  type: string;
  run_id?: string;
  payload: Record<string, unknown>;
  at: string;
};

type Bucket = {
  bucket: string;
  active_seconds: number;
  tokens_in: number;
  tokens_out: number;
  compute_seconds: number;
};

type Memory = {
  summaries: { topic: string; version: number; summary: string; created_by_run?: string }[];
  items: { topic: string; kind: string; content: string; source_run_id?: string }[];
};

const API_BASE = process.env.NEXT_PUBLIC_API_URL || "http://localhost:8080";

export default function SessionPage() {
  const { id } = useParams<{ id: string }>();
  const [events, setEvents] = useState<TimelineEvent[]>([]);
  const [buckets, setBuckets] = useState<Bucket[]>([]);
  const [memory, setMemory] = useState<Memory>({ summaries: [], items: [] });
  const [connState, setConnState] = useState("connecting");
  const [busy, setBusy] = useState<string>("");
  const lastSeq = useRef(0);

  // 时间轴：断线重连按 after=seq 续读（seq 允许 gap）
  useEffect(() => {
    const es = new EventSource(`${API_BASE}/sessions/${id}/events?after=${lastSeq.current}`);
    es.onopen = () => setConnState("open");
    es.onerror = () => setConnState("reconnecting");
    es.onmessage = (msg) => {
      const ev: TimelineEvent = JSON.parse(msg.data);
      lastSeq.current = ev.seq;
      setEvents((prev) => [...prev.slice(-199), ev]);
    };
    return () => es.close();
  }, [id]);

  useEffect(() => {
    fetch(`${API_BASE}/sessions/${id}/usage`)
      .then((r) => r.json())
      .then((d) => setBuckets(d.buckets || []))
      .catch(() => {});
  }, [id, events.length]);

  // 记忆面板：消化后展示（主题摘要 + 长期记忆条目）
  useEffect(() => {
    fetch(`${API_BASE}/sessions/${id}/memory`)
      .then((r) => r.json())
      .then((d) => setMemory({ summaries: d.summaries || [], items: d.items || [] }))
      .catch(() => {});
  }, [id, events.length]);

  // 人控状态推导：每个 run 的最新事件类型（awaiting_approval / frozen 即待操作）
  const runStates = useMemo(() => {
    const states: Record<string, string> = {};
    for (const ev of events) {
      if (ev.run_id) states[ev.run_id] = ev.type;
    }
    return states;
  }, [events]);

  // 群聊成员（派生自 group.turn 事件：agent_id + role + 发言次数）
  const participants = useMemo(() => {
    const map = new Map<string, { role: string; turns: number }>();
    for (const ev of events) {
      if (ev.type !== "group.turn") continue;
      const p = ev.payload as { agent_id?: string; role?: string };
      if (!p.agent_id) continue;
      const cur = map.get(p.agent_id) || { role: p.role || "", turns: 0 };
      cur.turns += 1;
      map.set(p.agent_id, cur);
    }
    return [...map.entries()];
  }, [events]);
  const pendingApproval = Object.entries(runStates).find(([, t]) => t === "run.awaiting_approval");
  const frozenRun = Object.entries(runStates).find(([, t]) => t === "run.frozen");

  const act = async (label: string, fn: () => Promise<Response>) => {
    setBusy(label);
    try {
      const r = await fn();
      if (!r.ok) throw new Error(await r.text());
    } catch (e) {
      console.error("action failed", e);
    } finally {
      setBusy("");
    }
  };

  return (
    <main style={{ fontFamily: "system-ui, sans-serif", padding: "2rem", maxWidth: 860, margin: "0 auto" }}>
      <p>
        <a href="/">← 会话列表</a>
      </p>
      <h1>会话 {id}</h1>
      <p style={{ color: "#666" }}>时间轴连接状态：{connState}</p>

      {/* 人控操作区（P2-2）：审批 / 解冻 */}
      {(pendingApproval || frozenRun) && (
        <div
          data-testid="human-controls"
          style={{ border: "1px solid #f0b429", background: "#fff8e6", borderRadius: 8, padding: 12, margin: "12px 0" }}
        >
          {pendingApproval && (
            <>
              <p style={{ margin: "0 0 8px" }}>
                <strong>等待人工审批</strong>（run {pendingApproval[0]}）——class 2 危险工具永不自动执行。
              </p>
              <button
                data-testid="approve-btn"
                disabled={busy !== ""}
                style={btnStyle("green")}
                onClick={() =>
                  act("approve", () =>
                    fetch(`${API_BASE}/webhooks/approval/${pendingApproval[0]}`, {
                      method: "POST",
                      headers: { "content-type": "application/json" },
                      body: JSON.stringify({ payload: "已批准" }),
                    }),
                  )
                }
              >
                批准执行
              </button>{" "}
              <button
                data-testid="reject-btn"
                disabled={busy !== ""}
                style={btnStyle("red")}
                onClick={() =>
                  act("reject", () =>
                    fetch(`${API_BASE}/webhooks/approval/${pendingApproval[0]}`, {
                      method: "POST",
                      headers: { "content-type": "application/json" },
                      body: JSON.stringify({ payload: JSON.stringify({ approved: false, note: "控制台拒绝" }) }),
                    }),
                  )
                }
              >
                拒绝
              </button>
            </>
          )}
          {frozenRun && (
            <>
              <p style={{ margin: "0 0 8px" }}>
                <strong>欠费冻结中</strong>（run {frozenRun[0]}）——充值预算后解冻继续执行。
              </p>
              <button
                data-testid="unfreeze-btn"
                disabled={busy !== ""}
                style={btnStyle("blue")}
                onClick={() =>
                  act("unfreeze", () =>
                    fetch(`${API_BASE}/sessions/${id}/actions`, {
                      method: "POST",
                      headers: { "content-type": "application/json" },
                      body: JSON.stringify({ action: "unfreeze" }),
                    }),
                  )
                }
              >
                解冻继续
              </button>
            </>
          )}
        </div>
      )}

      {participants.length > 0 && (
        <div data-testid="group-panel" style={{ border: "1px solid #7c3aed", background: "#f5f3ff", borderRadius: 8, padding: 12, margin: "12px 0" }}>
          <strong>群聊成员</strong>（moderator 主持循环 · group.turn 实时归属）
          <ul style={{ margin: "8px 0 0" }}>
            {participants.map(([agentID, info]) => (
              <li key={agentID} style={{ fontFamily: "monospace", fontSize: 13 }}>
                {info.role || "成员"} <span style={{ color: "#888" }}>({agentID.slice(0, 12)}…)</span> —— 发言 {info.turns} 次
              </li>
            ))}
          </ul>
        </div>
      )}

      <h2>时间轴回放（最近 200 条）</h2>
      <div data-testid="timeline" style={{ border: "1px solid #ddd", borderRadius: 8, maxHeight: 320, overflow: "auto", padding: 8 }}>
        {events.map((ev) => (
          <div key={ev.seq} style={{ fontFamily: "monospace", fontSize: 12, padding: "2px 0" }}>
            <span style={{ color: "#888" }}>{new Date(ev.at).toLocaleTimeString()}</span>{" "}
            <strong>{ev.type}</strong>{" "}
            <span style={{ color: "#555" }}>{JSON.stringify(ev.payload)}</span>
          </div>
        ))}
        {events.length === 0 && <p style={{ color: "#999" }}>暂无事件。</p>}
      </div>

      <h2>分层记忆（W5）</h2>
      <div data-testid="memory-panel">
        <h3>主题摘要</h3>
        {memory.summaries.length === 0 && <p style={{ color: "#999" }}>暂无摘要（对话量达阈值后自动消化）。</p>}
        {memory.summaries.map((s) => (
          <div key={`${s.topic}-${s.version}`} style={{ border: "1px solid #eee", borderRadius: 6, padding: 8, margin: "4px 0" }}>
            <strong>{s.topic}</strong> <span style={{ color: "#888" }}>v{s.version}</span>
            <p style={{ margin: "4px 0 0" }}>{s.summary}</p>
          </div>
        ))}
        <h3>长期记忆条目</h3>
        {memory.items.length === 0 && <p style={{ color: "#999" }}>暂无条目。</p>}
        <ul>
          {memory.items.map((it, i) => (
            <li key={i}>
              [{it.topic}] {it.content}
            </li>
          ))}
        </ul>
      </div>

      <h2>三轴计量（1min 桶）</h2>
      <table style={{ borderCollapse: "collapse", fontSize: 14 }}>
        <thead>
          <tr>
            {["桶", "活跃秒", "tokens_in", "tokens_out", "计算秒"].map((h) => (
              <th key={h} style={{ border: "1px solid #ddd", padding: 6 }}>{h}</th>
            ))}
          </tr>
        </thead>
        <tbody>
          {buckets.map((b) => (
            <tr key={b.bucket}>
              <td style={{ border: "1px solid #ddd", padding: 6 }}>{b.bucket}</td>
              <td style={{ border: "1px solid #ddd", padding: 6 }}>{b.active_seconds.toFixed(1)}</td>
              <td style={{ border: "1px solid #ddd", padding: 6 }}>{b.tokens_in}</td>
              <td style={{ border: "1px solid #ddd", padding: 6 }}>{b.tokens_out}</td>
              <td style={{ border: "1px solid #ddd", padding: 6 }}>{b.compute_seconds.toFixed(2)}</td>
            </tr>
          ))}
        </tbody>
      </table>
      {buckets.length === 0 && <p style={{ color: "#999" }}>暂无计量数据（聚合周期 1min）。</p>}
    </main>
  );
}

function btnStyle(color: "green" | "red" | "blue"): React.CSSProperties {
  const bg = { green: "#16a34a", red: "#dc2626", blue: "#2563eb" }[color];
  return { background: bg, color: "#fff", border: "none", borderRadius: 6, padding: "8px 14px", cursor: "pointer" };
}
