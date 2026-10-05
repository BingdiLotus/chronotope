"use client";

// W4 会话页：时间轴回放（SSE after=seq）+ 三轴计量。
import { useEffect, useRef, useState } from "react";
import { useParams } from "next/navigation";

type TimelineEvent = {
  seq: number;
  type: string;
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

export default function SessionPage() {
  const { id } = useParams<{ id: string }>();
  const [events, setEvents] = useState<TimelineEvent[]>([]);
  const [buckets, setBuckets] = useState<Bucket[]>([]);
  const [connState, setConnState] = useState("connecting");
  const lastSeq = useRef(0);

  // 时间轴：断线重连按 after=seq 续读（seq 允许 gap）
  useEffect(() => {
    const es = new EventSource(`/api/sessions/${id}/events?after=${lastSeq.current}`);
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
    fetch(`/api/sessions/${id}/usage`)
      .then((r) => r.json())
      .then((d) => setBuckets(d.buckets || []))
      .catch(() => {});
  }, [id, events.length]);

  return (
    <main style={{ fontFamily: "system-ui, sans-serif", padding: "2rem", maxWidth: 860, margin: "0 auto" }}>
      <p>
        <a href="/">← 会话列表</a>
      </p>
      <h1>会话 {id}</h1>
      <p style={{ color: "#666" }}>时间轴连接状态：{connState}</p>

      <h2>时间轴回放（最近 200 条）</h2>
      <div style={{ border: "1px solid #ddd", borderRadius: 8, maxHeight: 320, overflow: "auto", padding: 8 }}>
        {events.map((ev) => (
          <div key={ev.seq} style={{ fontFamily: "monospace", fontSize: 12, padding: "2px 0" }}>
            <span style={{ color: "#888" }}>{new Date(ev.at).toLocaleTimeString()}</span>{" "}
            <strong>{ev.type}</strong>{" "}
            <span style={{ color: "#555" }}>{JSON.stringify(ev.payload)}</span>
          </div>
        ))}
        {events.length === 0 && <p style={{ color: "#999" }}>暂无事件。</p>}
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
