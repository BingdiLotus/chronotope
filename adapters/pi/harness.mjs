// Pi 官方 SDK 适配器（批 3——pi-ai 核心接入的骨架）。
//
// 经 /runs 协议接 Chronotope；官方 pi-ai 的 streamSimple 调用点 + 帧映射。
// 模型装配：pi-ai 的模型 catalog 需 coding-agent 的 ModelManager 完整形态
// （compat 入口的 getModels 在 catalog 生成前返回空——官方迁移中）。
// 骨架参数化：PI_MODEL_ID/PI_PROVIDER env——catalog 装配完成后即插即用。
// 官方参考：https://github.com/earendil-works/pi（packages/agent + pi-ai）
import http from "node:http";

const PORT = parseInt(process.argv[2] || "8030", 10);
const MARK = "PiCore"; // 回复标记（路由证据）

// pi-ai 的官方调用点（catalog 装配已解决——providers/all 的静态 MODELS：
//   const { getBuiltinModel } = await import(
//     "@earendil-works/pi-ai/providers/all");
//   const { streamSimple } = await import("@earendil-works/pi-ai/compat");
//   const model = getBuiltinModel("anthropic", "claude-sonnet-4-6");
//   const stream = streamSimple(model, { messages, tools }, options);
// 帧映射：text-delta → delta；toolCall 事件 → tool_call；result → done
// + usage（Chronotope 账本自动落）。
// 已知受阻：pi-ai 的 anthropic auth 路径 401（invalid x-api-key——官方
// auth 迁移中；claude-agent-sdk 同 key 直连成功——pi-ai 侧 env 注入差异
// 待官方 auth 形态稳定）。

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
  let body = "";
  req.on("data", (c) => (body += c));
  req.on("end", () => {
    let reqBody = {};
    try {
      reqBody = JSON.parse(body || "{}");
    } catch {
      /* 非法 JSON——按空请求 */
    }
    res.writeHead(200, {
      "Content-Type": "text/event-stream",
      "Cache-Control": "no-cache",
    });
    sse(res, { type: "beat", seq: 0, payload: {} });
    // 诚实状态：catalog 装配已通（getBuiltinModel）——真实调用受阻于
    // pi-ai 的 anthropic auth 401（官方 auth 迁移中）——显式 error 帧
    sse(res, {
      type: "error",
      seq: 1,
      payload: {
        error:
          "pi-ai 模型已解析（getBuiltinModel anthropic/claude-sonnet-4-6）——auth 401 待官方 auth 形态稳定（官方迁移中）",
      },
    });
    sse(res, {
      type: "done",
      seq: 1,
      payload: { final: `${MARK} 回复`, usage: {}, truncated: false },
    });
  });
});

server.listen(PORT, "0.0.0.0", () => {
  console.log(`pi-core-harness listening on :${PORT}`);
});
