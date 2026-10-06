import { expect, test } from "@playwright/test";

// 控制台 e2e（P2-2 人控闭环）：时间轴渲染、记忆面板、审批批准/拒绝交互。
// 前置：平台 API 运行于 API_URL（默认 localhost:8080），harness 处于审批脚本模式
// （见 scripts/console-e2e.sh 一键编排）。

const API = process.env.API_URL || "http://localhost:8080";

async function api(path: string, init?: RequestInit) {
  const r = await fetch(`${API}${path}`, {
    headers: { "content-type": "application/json", ...(init?.headers || {}) },
    ...init,
  });
  if (!r.ok) throw new Error(`${r.status}: ${await r.text()}`);
  return r.json();
}

// 每测独立会话：同会话双开 409（边界语义 §6）+ 挂起中提交不返回 → 互不干扰
let sessions: string[] = [];

test.beforeAll(async () => {
  const org = `org-console-${Date.now()}`;
  const agent = await api(`/orgs/${org}/agents`, {
    method: "POST",
    body: JSON.stringify({
      name: "console-agent",
      config: {
        model: "claude-sonnet-4-6",
        instructions: "你是控制台演示助手。",
        tools: ["bash"],
        tool_classes: { bash: 2 }, // class 2：强制审批 → 页面出现批准/拒绝按钮
        version: 1,
      },
    }),
  });
  for (let i = 0; i < 3; i++) {
    const sess = await api(`/agents/${agent.id}/sessions`, { method: "POST" });
    sessions.push(sess.id);
  }
});

test("会话页渲染时间轴与记忆面板", async ({ page }) => {
  // 提交一个挂起审批的 run（fire-and-forget：挂起中提交请求不返回，绝不可 await）
  api(`/sessions/${sessions[0]}/runs`, {
    method: "POST",
    headers: { "Idempotency-Key": `console-${Date.now()}` },
    body: JSON.stringify({ input: "执行危险命令" }),
  }).catch(() => {});

  await page.goto(`/sessions/${sessions[0]}`);
  await expect(page.getByTestId("timeline")).toBeVisible();
  await expect(page.getByTestId("memory-panel")).toBeVisible();

  // 等待审批事件流入 → 人控操作区出现
  await expect(page.getByTestId("human-controls")).toBeVisible({ timeout: 30_000 });
});

test("批准：点击后 run 恢复执行并完成", async ({ page }) => {
  api(`/sessions/${sessions[1]}/runs`, {
    method: "POST",
    headers: { "Idempotency-Key": `console-ap-${Date.now()}` },
    body: JSON.stringify({ input: "执行危险命令" }),
  }).catch(() => {});

  await page.goto(`/sessions/${sessions[1]}`);
  await expect(page.getByTestId("approve-btn")).toBeVisible({ timeout: 30_000 });
  await page.getByTestId("approve-btn").click();
  // run 恢复执行 → 时间轴出现 run.completed（sandbox 执行 + 终答）
  await expect(page.getByTestId("timeline")).toContainText("run.completed", { timeout: 30_000 });
  // 操作区随终态消失（该 run 已不在待审批状态）
  await expect(page.getByTestId("human-controls")).toHaveCount(0, { timeout: 15_000 });
});

test("拒绝：点击后 run 以 tool_denied 终止且不执行", async ({ page }) => {
  api(`/sessions/${sessions[2]}/runs`, {
    method: "POST",
    headers: { "Idempotency-Key": `console-dn-${Date.now()}` },
    body: JSON.stringify({ input: "执行危险命令" }),
  }).catch(() => {});

  await page.goto(`/sessions/${sessions[2]}`);
  await expect(page.getByTestId("reject-btn")).toBeVisible({ timeout: 30_000 });
  await page.getByTestId("reject-btn").click();
  await expect(page.getByTestId("timeline")).toContainText("audit.tool_denied", { timeout: 30_000 });
  await expect(page.getByTestId("timeline")).toContainText("tool_denied", { timeout: 30_000 });
});
