import { expect, test } from "@playwright/test";

// 时空视图 e2e（期 2 §C）：checkpoint 标记 / fork 血缘 / rollback 审计 / diff 差集。
// 前置：平台 API 运行于 API_URL（默认 localhost:8080）；harness fake plain 模式。
// 编排：scripts/console-e2e.sh tt（起 api + fake harness + web + 跑本测试）。

const API = process.env.API_URL || "http://localhost:8080";

async function api(path: string, init?: RequestInit) {
  const r = await fetch(`${API}${path}`, {
    headers: { "content-type": "application/json", ...(init?.headers || {}) },
    ...init,
  });
  if (!r.ok) throw new Error(`${r.status}: ${await r.text()}`);
  return r.json();
}

let parent: string;

test.beforeAll(async () => {
  const org = `org-tt-${Date.now()}`;
  const agent = await api(`/orgs/${org}/agents`, {
    method: "POST",
    body: JSON.stringify({
      name: "tt-agent",
      config: { model: "claude-sonnet-4-6", instructions: "你是时间旅行助手。", tools: [], version: 1 },
    }),
  });
  const sess = await api(`/agents/${agent.id}/sessions`, { method: "POST" });
  parent = sess.id;
  // 首轮 run（fake plain 完成）→ 产生事件
  await api(`/sessions/${parent}/runs`, {
    method: "POST",
    headers: { "Idempotency-Key": `tt-${Date.now()}` },
    body: JSON.stringify({ input: "首轮对话" }),
  });
});

test("创建检查点 → fork → 血缘可见 → diff 差集", async ({ page }) => {
  await page.goto(`/sessions/${parent}`);
  await expect(page.getByTestId("timetravel-panel")).toBeVisible();

  // 创建检查点 → 列表出现 + 时间轴出现 checkpoint 标记
  await page.getByTestId("create-checkpoint-btn").click();
  await expect(page.getByTestId("checkpoint-list")).toBeVisible({ timeout: 15_000 });
  await expect(page.getByTestId("checkpoint-marker").first()).toBeVisible({ timeout: 15_000 });

  // 次轮 run（checkpoint 之后的新事实）
  await api(`/sessions/${parent}/runs`, {
    method: "POST",
    headers: { "Idempotency-Key": `tt2-${Date.now()}` },
    body: JSON.stringify({ input: "次轮对话" }),
  });

  // fork → 结果链接出现 → 跳转分支页 → 血缘面包屑可见
  const forkBtn = page.getByTestId(/^fork-/).first();
  await forkBtn.click();
  await expect(page.getByTestId("fork-result")).toBeVisible({ timeout: 15_000 });
  const forkLink = page.getByTestId("fork-result").locator("a");
  const forkID = (await forkLink.getAttribute("href"))!.replace("/sessions/", "");

  await page.goto(`/sessions/${forkID}`);
  await expect(page.getByTestId("forked-breadcrumb")).toBeVisible({ timeout: 15_000 });

  // 回父会话 → diff（对比 fork 会话）
  await page.goto(`/sessions/${parent}`);
  await page.getByTestId("diff-input").fill(forkID);
  await page.getByTestId("diff-btn").click();
  await expect(page.getByTestId("diff-view")).toBeVisible({ timeout: 15_000 });
  await expect(page.getByTestId("diff-view")).toContainText("本会话独有");
  await expect(page.getByTestId("diff-view")).toContainText("session.forked");

  // rollback → 时间轴出现 rolled_back 审计事件（真相不可变）
  const rollbackBtn = page.getByTestId(/^rollback-/).first();
  await rollbackBtn.click();
  await expect(page.getByTestId("timeline")).toContainText("session.rolled_back", { timeout: 15_000 });
});
