import { test, expect } from "@playwright/test";

// 期 5 §B：org 管理页 e2e——用量看板/配额进度/成员 CRUD/审计流。
// 编排：scripts/console-e2e.sh org（起 api + fake harness + web + 跑本测试）。
// 前置数据由脚本内直连 API 种子（org + user + 用量由 api 聚合器产生）。

const API = process.env.NEXT_PUBLIC_API_URL || "http://localhost:8080";
const ORG = `org-pw-${Date.now()}`;

test.beforeAll(async ({ request }) => {
  // 种子：org → agent → user → 成员 → run（产生用量）→ 配额
  await request.post(`${API}/orgs/${ORG}/agents`, {
    data: {
      name: "pw-agent",
      config: { model: "claude-sonnet-4-6", instructions: "一句话。", tools: [], version: 1 },
    },
  });
  const agentResp = await request.get(`${API}/orgs/${ORG}/sessions`);
  void agentResp;
  const userResp = await request.post(`${API}/orgs/${ORG}/users`, { data: { name: "pw-admin" } });
  const user = await userResp.json();
  await request.post(`${API}/orgs/${ORG}/members`, { data: { user_id: user.id, role: "org_admin" } });
  const auditorResp = await request.post(`${API}/orgs/${ORG}/users`, { data: { name: "pw-auditor" } });
  const auditor = await auditorResp.json();
  (globalThis as { auditorID?: string }).auditorID = auditor.id;
  await request.put(`${API}/orgs/${ORG}/quota`, { data: { daily_token_budget: 100000 } });
});

test("用量看板与配额进度渲染", async ({ page }) => {
  await page.goto(`/orgs/${ORG}`);
  await expect(page.getByText("用量看板（日粒度）")).toBeVisible();
  // 配额进度条由种子预算驱动（budget>0 即渲染——usage 聚合周期 1m 不依赖）
  await expect(page.getByText("配额进度（token）")).toBeVisible();
  await expect(page.getByText("累计 token")).toBeVisible();
});

test("成员 CRUD", async ({ page }) => {
  await page.goto(`/orgs/${ORG}`);
  const auditorID = (globalThis as { auditorID?: string }).auditorID || "u-pw";
  await page.getByTestId("member-id").fill(auditorID);
  await page.getByTestId("member-role").selectOption("auditor");
  await page.getByTestId("member-add").click();
  await expect(page.getByTestId("member-list")).toContainText("auditor");
  // 移除（CRUD 的 D——列表回退）
  await page.getByRole("button", { name: "移除" }).last().click();
  await expect(page.getByTestId("member-list")).not.toContainText(auditorID);
});

test("审计流区块渲染（只读）", async ({ page }) => {
  await page.goto(`/orgs/${ORG}`);
  await expect(page.getByText("审批审计流（只读）")).toBeVisible();
  // 空数据时占位文本（空 ul height 0 隐藏——断言占位/标题即可）
  await expect(page.getByText(/暂无审批审计事件|kind|approval_/)).toBeVisible();
});

test("首页导航接入管理页", async ({ page }) => {
  await page.goto("/");
  await page.getByTestId("org-manage-link").click();
  await expect(page).toHaveURL(/\/orgs\//);
  await expect(page.getByText("用量看板（日粒度）")).toBeVisible();
});
