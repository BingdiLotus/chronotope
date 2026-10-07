import { expect, test } from "@playwright/test";

// run 树拓扑图 e2e（期 2 §C 后置 #3）：subagent.spawned 派生边 + 终态渲染。
// 前置：平台 API 运行于 API_URL；harness 带 spawn_subagent 脚本。
// 编排：scripts/console-e2e.sh runtree。

const API = process.env.API_URL || "http://localhost:8080";

async function api(path: string, init?: RequestInit) {
  const r = await fetch(`${API}${path}`, {
    headers: { "content-type": "application/json", ...(init?.headers || {}) },
    ...init,
  });
  if (!r.ok) throw new Error(`${r.status}: ${await r.text()}`);
  return r.json();
}

let sessionID: string;

test.beforeAll(async () => {
  const org = `org-rt-${Date.now()}`;
  const parent = await api(`/orgs/${org}/agents`, {
    method: "POST",
    body: JSON.stringify({
      name: "rt-parent",
      config: { model: "claude-sonnet-4-6", instructions: "父任务助手。", tools: ["spawn_subagent"], version: 1 },
    }),
  });
  const sess = await api(`/agents/${parent.id}/sessions`, { method: "POST" });
  sessionID = sess.id;
  // 提交一个 spawn 子任务的 run（父 run 派生 child run）
  await api(`/sessions/${sessionID}/runs`, {
    method: "POST",
    headers: { "Idempotency-Key": `rt-${Date.now()}` },
    body: JSON.stringify({ input: "派生子任务" }),
  });
});

test("run 树面板渲染派生边与子任务终态", async ({ page }) => {
  await page.goto(`/sessions/${sessionID}`);
  await expect(page.getByTestId("run-tree")).toBeVisible({ timeout: 30_000 });
  // 派生边（父 → spawn 子）+ 子任务完成状态
  await expect(page.getByTestId("run-tree")).toContainText("→ spawn", { timeout: 15_000 });
  await expect(page.getByTestId("run-tree")).toContainText("subagent.completed", { timeout: 30_000 });
});
