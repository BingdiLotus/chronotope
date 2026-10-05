// W4 控制台骨架：会话列表 / 时间轴回放 / 用量三轴（mvp-落地方案 §4 W4）。
export default function Home() {
  return (
    <main style={{ fontFamily: "system-ui, sans-serif", padding: "2rem" }}>
      <h1>Chronotope Console</h1>
      <p>时空可组合持久运行时 · 控制台骨架（W4 实现）</p>
      <ul>
        <li>会话列表：GET /sessions（待实现）</li>
        <li>时间轴回放：GET /sessions/:id/events（SSE，after=seq 续读）</li>
        <li>用量三轴：活跃秒 / token / 计算秒（1min 桶聚合）</li>
      </ul>
    </main>
  );
}
