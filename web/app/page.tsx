"use client";

// W4 控制台最小页：会话列表（默认 org 可经查询参数切换）。
import { useEffect, useState } from "react";

type Session = {
  id: string;
  agent_id: string;
  status: string;
  last_active_at: string | null;
};

export default function Home() {
  const [org, setOrg] = useState("org-demo");
  const [sessions, setSessions] = useState<Session[]>([]);
  const [error, setError] = useState("");

  useEffect(() => {
    fetch(`${process.env.NEXT_PUBLIC_API_URL || "http://localhost:8080"}/orgs/${org}/sessions`)
      .then((r) => r.json())
      .then((d) => setSessions(d.sessions || []))
      .catch((e) => setError(String(e)));
  }, [org]);

  return (
    <main style={{ fontFamily: "system-ui, sans-serif", padding: "2rem", maxWidth: 720, margin: "0 auto" }}>
      <h1>Chronotope Console</h1>
      <p style={{ color: "#666" }}>时空可组合持久运行时 · 控制台（W4）</p>
      <label>
        组织：
        <input value={org} onChange={(e) => setOrg(e.target.value)} style={{ marginLeft: 8, padding: 4 }} />
      </label>
      {error && <p style={{ color: "red" }}>{error}</p>}
      <ul>
        {sessions.map((s) => (
          <li key={s.id} style={{ margin: "8px 0" }}>
            <a href={`/sessions/${s.id}`}>{s.id}</a>
            <span style={{ color: "#888", marginLeft: 12 }}>{s.status}</span>
            <span style={{ color: "#aaa", marginLeft: 12 }}>
              {s.last_active_at ? new Date(s.last_active_at).toLocaleString() : "-"}
            </span>
          </li>
        ))}
      </ul>
      {sessions.length === 0 && <p style={{ color: "#999" }}>暂无会话（可经 API 创建，见 README 快速开始）。</p>}
    </main>
  );
}
