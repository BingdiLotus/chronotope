"use client";

// 会话页（W4 控制台 + P2-2 人控闭环 + 期 2 §C 时空视图）：时间轴回放
// （SSE after=seq + checkpoint 标记）+ 人控操作 + 时空操作（checkpoint 树 /
// fork / rollback / diff）+ 分层记忆面板 + 三轴计量。
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
  summaries: { topic: string; version: number; summary: string }[];
  items: { topic: string; kind: string; content: string }[];
};

type Checkpoint = {
  id: string;
  seq: number;
  snapshot_ref: string;
};

type SessionMeta = {
  id: string;
  forked_from_session?: string;
  forked_at_seq?: number;
};

type Diff = {
  common_prefix: number;
  only_a: { seq: number; type: string; payload: Record<string, unknown>; at: string }[];
  only_b: { seq: number; type: string; payload: Record<string, unknown>; at: string }[];
};

const API_BASE = process.env.NEXT_PUBLIC_API_URL || "http://localhost:8080";

export default function SessionPage() {
  const { id } = useParams<{ id: string }>();
  const [events, setEvents] = useState<TimelineEvent[]>([]);
  const [buckets, setBuckets] = useState<Bucket[]>([]);
  const [memory, setMemory] = useState<Memory>({ summaries: [], items: [] });
  const [meta, setMeta] = useState<SessionMeta | null>(null);
  const [checkpoints, setCheckpoints] = useState<Checkpoint[]>([]);
  const [connState, setConnState] = useState("connecting");
  const [busy, setBusy] = useState<string>("");
  const [notice, setNotice] = useState("");
  const [forkResult, setForkResult] = useState<{ session_id: string; at_seq: number } | null>(null);
  const [diffAgainst, setDiffAgainst] = useState("");
  const [diff, setDiff] = useState<Diff | null>(null);
  const lastSeq = useRef(0);

  const refreshCheckpoints = () => {
    fetch(`${API_BASE}/sessions/${id}/checkpoints`)
      .then((r) => r.json())
      .then((d) => setCheckpoints(d.checkpoints || []))
      .catch(() => {});
  };

  // 时间轴：断线重连按 after=seq 续读（seq 允许 gap）
  useEffect(() => {
    const es = new EventSource(`${API_BASE}/sessions/${id}/events?after=${lastSeq.current}`);
    es.onopen = () => setConnState("open");
    es.onerror = () => setConnState("reconnecting");
    es.onmessage = (msg) => {
      const ev: TimelineEvent = JSON.parse(msg.data);
      lastSeq.current = ev.seq;
      setEvents((prev) => [...prev.slice(-199), ev]);
      if (ev.type === "session.checkpoint" || ev.type === "session.rolled_back" || ev.type === "session.forked") {
        refreshCheckpoints();
      }
    };
    return () => es.close();
  }, [id]);

  useEffect(() => {
    fetch(`${API_BASE}/sessions/${id}`)
      .then((r) => r.json())
      .then((d) => setMeta(d))
      .catch(() => {});
    refreshCheckpoints();
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

  // run 树（期 2 §C 后置 #3）：组合关系入时间轴——subagent.spawned 派生边 +
  // 各 run 终态（父会话视角；子 run 的完整轨迹在子会话时间轴）
  const runTree = useMemo(() => {
    const status: Record<string, string> = {};
    const edges: { parent: string; child: string; childSession: string; agent: string }[] = [];
    for (const ev of events) {
      if (ev.run_id && ["run.started", "run.completed", "run.failed", "run.cancelled"].includes(ev.type)) {
        status[ev.run_id] = ev.type;
      }
      if (ev.type === "subagent.spawned" && ev.run_id) {
        const p = ev.payload as { child_run_id?: string; child_session_id?: string; agent?: string };
        if (p.child_run_id) {
          edges.push({ parent: ev.run_id, child: p.child_run_id, childSession: p.child_session_id || "", agent: p.agent || "" });
        }
      }
      if ((ev.type === "subagent.completed" || ev.type === "subagent.failed") && ev.run_id) {
        const p = ev.payload as { child_run_id?: string };
        if (p.child_run_id) status[p.child_run_id] = ev.type;
      }
    }
    return { status, edges };
  }, [events]);

  const act = async (label: string, fn: () => Promise<Response>, after?: (r: Response) => void) => {
    setBusy(label);
    setNotice("");
    try {
      const r = await fn();
      if (!r.ok) throw new Error(await r.text());
      if (after) after(r);
      setNotice(`✓ ${label} 完成`);
    } catch (e) {
      setNotice(`✗ ${label} 失败：${e instanceof Error ? e.message : String(e)}`);
    } finally {
      setBusy("");
    }
  };

  const createCheckpoint = () =>
    act("创建检查点", () =>
      fetch(`${API_BASE}/sessions/${id}/checkpoints`, { method: "POST", headers: { "content-type": "application/json" }, body: "{}" }),
    ).then(refreshCheckpoints);

  const forkAt = (cp: Checkpoint) =>
    act("Fork 分支", () =>
      fetch(`${API_BASE}/sessions/${id}/fork`, {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ checkpoint_id: cp.id }),
      }),
      (r) => r.json().then((d) => setForkResult(d)),
    );

  const rollbackTo = (cp: Checkpoint) =>
    act("回退", () =>
      fetch(`${API_BASE}/sessions/${id}/rollback`, {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ checkpoint_id: cp.id }),
      }),
    ).then(refreshCheckpoints);

  const runDiff = () =>
    act(
      "对比",
      () => fetch(`${API_BASE}/sessions/${id}/diff?against=${encodeURIComponent(diffAgainst)}`),
      (r) => r.json().then((d) => setDiff(d)),
    );

  return (
    <main style={{ fontFamily: "system-ui, sans-serif", padding: "2rem", maxWidth: 860, margin: "0 auto" }}>
      <p>
        <a href="/">← 会话列表</a>
      </p>
      <h1>会话 {id}</h1>
      <p style={{ color: "#666" }}>
        时间轴连接状态：{connState}
        {meta?.forked_from_session && (
          <>
            {" · "}分支自{" "}
            <a href={`/sessions/${meta.forked_from_session}`} data-testid="forked-breadcrumb">
              {meta.forked_from_session.slice(0, 12)}…@{meta.forked_at_seq}
            </a>
          </>
        )}
      </p>

      {/* 时空操作（期 2 §C：checkpoint 树 + fork/rollback/diff） */}
      <div data-testid="timetravel-panel" style={{ border: "1px solid #2563eb", background: "#eff6ff", borderRadius: 8, padding: 12, margin: "12px 0" }}>
        <strong>时空视图</strong>（checkpoint(session, seq) 钉住时间×空间；真相不可变——回退追加审计事件）
        <div style={{ margin: "8px 0" }}>
          <button data-testid="create-checkpoint-btn" disabled={busy !== ""} style={btnStyle("blue")} onClick={createCheckpoint}>
            创建检查点
          </button>{" "}
          {notice && <span style={{ color: notice.startsWith("✓") ? "#16a34a" : "#dc2626", marginLeft: 8 }}>{notice}</span>}
        </div>
        {checkpoints.length > 0 && (
          <ul data-testid="checkpoint-list" style={{ margin: "8px 0 0", paddingLeft: 20, fontSize: 14 }}>
            {checkpoints.map((cp) => (
              <li key={cp.id} style={{ margin: "4px 0" }} data-testid={`checkpoint-${cp.id}`}>
                <span style={{ fontFamily: "monospace" }}>{cp.id.slice(0, 14)}…</span>{" "}
                <span style={{ color: "#888" }}>seq={cp.seq}</span>{" "}
                <span style={{ color: "#aaa" }}>{cp.snapshot_ref ? `快照 ${cp.snapshot_ref.slice(0, 18)}…` : "无快照"}</span>{" "}
                <button data-testid={`fork-${cp.id}`} disabled={busy !== ""} style={btnStyle("blue")} onClick={() => forkAt(cp)}>
                  Fork
                </button>{" "}
                <button data-testid={`rollback-${cp.id}`} disabled={busy !== ""} style={btnStyle("red")} onClick={() => rollbackTo(cp)}>
                  回退
                </button>
              </li>
            ))}
          </ul>
        )}
        {checkpoints.length === 0 && <p style={{ color: "#999", fontSize: 13 }}>暂无检查点。</p>}
        {forkResult && (
          <p data-testid="fork-result" style={{ fontSize: 14 }}>
            已派生分支：<a href={`/sessions/${forkResult.session_id}`}>{forkResult.session_id}</a>（at_seq={forkResult.at_seq}）
          </p>
        )}
        <div style={{ marginTop: 8, fontSize: 14 }}>
          对比分支会话：
          <input
            data-testid="diff-input"
            value={diffAgainst}
            onChange={(e) => setDiffAgainst(e.target.value)}
            placeholder="分支会话 id"
            style={{ margin: "0 8px", padding: 4, width: 220, fontFamily: "monospace" }}
          />
          <button data-testid="diff-btn" disabled={busy !== "" || diffAgainst === ""} style={btnStyle("blue")} onClick={runDiff}>
            对比
          </button>
        </div>
        {diff && (
          <div data-testid="diff-view" style={{ marginTop: 8, fontSize: 13, borderTop: "1px solid #c7d2fe", paddingTop: 8 }}>
            公共前缀 {diff.common_prefix} 条 · 本会话独有 {diff.only_a.length} · 对方独有 {diff.only_b.length}
            <div style={{ display: "flex", gap: 12, marginTop: 6 }}>
              <div style={{ flex: 1 }}>
                <strong>本会话</strong>
                {diff.only_a.map((e) => (
                  <div key={`a-${e.seq}`} style={{ fontFamily: "monospace", fontSize: 12 }}>
                    {e.type}
                  </div>
                ))}
              </div>
              <div style={{ flex: 1 }}>
                <strong>分支</strong>
                {diff.only_b.map((e) => (
                  <div key={`b-${e.seq}`} style={{ fontFamily: "monospace", fontSize: 12 }}>
                    {e.type}
                  </div>
                ))}
              </div>
            </div>
          </div>
        )}
      </div>

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

      {runTree.edges.length > 0 && (
        <div data-testid="run-tree" style={{ border: "1px solid #0891b2", background: "#ecfeff", borderRadius: 8, padding: 12, margin: "12px 0" }}>
          <strong>run 树</strong>（组合关系：父 run → 派生子 run；状态随事件实时）
          <ul style={{ margin: "8px 0 0", paddingLeft: 20, fontSize: 14 }}>
            {runTree.edges.map((e, i) => (
              <li key={i} style={{ fontFamily: "monospace", margin: "4px 0" }}>
                {e.parent.slice(0, 10)}… <span style={{ color: "#0891b2" }}>→ spawn</span> {e.child.slice(0, 10)}…
                <span style={{ color: "#666" }}>（{e.agent}）</span>{" "}
                <span style={{ color: (runTree.status[e.child] || "running") === "subagent.completed" ? "#16a34a" : "#888" }}>
                  [{runTree.status[e.child] || "running"}]
                </span>
              </li>
            ))}
          </ul>
        </div>
      )}

      <h2>时间轴回放（最近 200 条）</h2>
      <div data-testid="timeline" style={{ border: "1px solid #ddd", borderRadius: 8, maxHeight: 320, overflow: "auto", padding: 8 }}>
        {events.map((ev) =>
          ev.type === "session.checkpoint" ? (
            <div
              key={ev.seq}
              data-testid="checkpoint-marker"
              style={{ fontFamily: "monospace", fontSize: 12, padding: "2px 0", background: "#dbeafe", borderLeft: "4px solid #2563eb", margin: "2px 0" }}
            >
              <span style={{ color: "#888" }}>{new Date(ev.at).toLocaleTimeString()}</span>{" "}
              <strong>⏱ checkpoint</strong>{" "}
              <span style={{ color: "#555" }}>{JSON.stringify(ev.payload)}</span>
            </div>
          ) : (
            <div key={ev.seq} style={{ fontFamily: "monospace", fontSize: 12, padding: "2px 0" }}>
              <span style={{ color: "#888" }}>{new Date(ev.at).toLocaleTimeString()}</span>{" "}
              <strong>{ev.type}</strong>{" "}
              <span style={{ color: "#555" }}>{JSON.stringify(ev.payload)}</span>
            </div>
          ),
        )}
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
  return { background: bg, color: "#fff", border: "none", borderRadius: 6, padding: "6px 12px", cursor: "pointer", fontSize: 13 };
}
