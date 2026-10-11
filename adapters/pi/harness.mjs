// Pi 官方 SDK 适配器（批 3——pi-durable 完整形态重构）。
//
// 与 Cloudflare PiHarness 同款官方形态：Harness.open(MemoryStorage) +
// root.submit → wait → AssistantEntry——MemoryStorage 是 transient（适配器
// 进程内——每次 run 重建——三铁律：Pi 状态不持久化到 Chronotope）。
// 官方参考：@earendil-works/pi-durable README（Quick Start）。
//
// 已探明的官方装配（2026-10-11）：
//   createProvider({ id, baseUrl, auth: { apiKey: envApiKeyAuth(...) },
//                    models, api }) → createModels().setProvider
//   Harness.open(new MemoryStorage(), { models, registry: createRegistry() })
//   root(ctx, { agent: { model: { provider, modelId } } })
//   submit({ type:"input", content }) → wait → status done/unanswered
//   commit((tx) => tx.entry(AssistantEntry, settled.answer))
//
// 已知剩余：poke2api 代理的 key 校验（curl 的 Bearer 直连成功 vs pi-ai
// 的 401——pi-ai 请求构造的 header/URL 细节差异；官方 provider 硬编码
// 官方端点——baseUrl 参数已覆盖。env 注入本身可行：envApiKeyAuth
// 官方工厂读 OPENAI_API_KEY ✓）。
import http from "node:http";

const PORT = parseInt(process.argv[2] || "8030", 10);
const MARK = "PiDurable"; // 回复标记（路由证据）

const { createProvider, createModels, envApiKeyAuth, openAICompletionsApi } =
  await import("@earendil-works/pi-ai/compat");
const { getBuiltinModel } = await import("@earendil-works/pi-ai/providers/all");
const { Harness, MemoryStorage, createRegistry, AssistantEntry } =
  await import("@earendil-works/pi-durable");

// 每次 run 重建的官方装配（transient——MemoryStorage）
async function runPi(body) {
  const messages = body.messages || [];
  const prompt = messages
    .filter((m) => m.role === "user")
    .map((m) => m.content)
    .join("\n");
  const base = getBuiltinModel("openai", "gpt-6.1-sol");
  const provider = createProvider({
    id: "openai-proxy",
    baseUrl: process.env.OPENAI_BASE_URL || "https://api.openai.com/v1",
    headers: { Authorization: `Bearer ${process.env.OPENAI_API_KEY || ""}` },
    auth: { apiKey: envApiKeyAuth("key", ["OPENAI_API_KEY"]), oauth: undefined },
    models: [{ ...base, api: "openai-completions", provider: "openai-proxy" }],
    api: { "openai-completions": openAICompletionsApi() },
  });
  const models = createModels();
  models.setProvider(provider);
  const harness = await Harness.open(
    new MemoryStorage(),
    { models, registry: createRegistry() },
    {}
  );
  try {
    const root = await harness.root({}, {
      agent: { model: { provider: "openai-proxy", modelId: "gpt-6.1-sol" } },
    });
    const submission = await root.submit({ type: "input", content: prompt }, {});
    const settled = await submission.wait({});
    if (settled.status === "done" && settled.type === "input") {
      const answer = await root.commit(
        (tx) => tx.entry(AssistantEntry, settled.answer),
        {}
      );
      const text = (answer?.model || []).map(String).join(" ");
      return { final: `${text} [${MARK}]`.trim(), usage: {}, err: null };
    }
    return { final: "", usage: {}, err: `pi-durable: ${settled.reason}: ${String(settled.detail || "").slice(0, 200)}` };
  } finally {
    await harness.close({});
  }
}

function sse(res, obj) {
  res.write(`data: ${JSON.stringify(obj)}\n\n`);
}

const server = http.createServer((req, res) => {
  if (req.method === "GET" && req.url === "/healthz") {
    res.writeHead(200).end("ok");
    return;
  }
  if (req.method !== "POST" || req.url !== "/runs") {
    res.writeHead(404).end();
    return;
  }
  let raw = "";
  req.on("data", (c) => (raw += c));
  req.on("end", async () => {
    let body = {};
    try {
      body = JSON.parse(raw || "{}");
    } catch {}
    res.writeHead(200, {
      "Content-Type": "text/event-stream",
      "Cache-Control": "no-cache",
    });
    sse(res, { type: "beat", seq: 0, payload: {} });
    try {
      const { final, err } = await runPi(body);
      if (err) {
        sse(res, { type: "error", seq: 1, payload: { error: err } });
        return;
      }
      sse(res, { type: "delta", seq: 1, payload: { text: final } });
      sse(res, { type: "done", seq: 2, payload: { final, usage: {}, truncated: false } });
    } catch (e) {
      sse(res, { type: "error", seq: 1, payload: { error: String(e) } });
    }
  });
});

server.listen(PORT, "0.0.0.0", () => {
  console.log(`pi-durable-harness listening on :${PORT}`);
});
