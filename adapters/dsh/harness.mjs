// dsh 官方 SDK 适配器（批 4——@deepseek-ai/dsh-sdk-client 骨架）。
//
// 经 /runs 协议接 Chronotope；官方 DeepSeekHarness 的 owned-run API：
//   new DeepSeekHarness({ launch: { command, args }, provider, model,
//                         maxTokens }) → run(prompt) → finalResponse
// HarnessClient 底层协议（start/initialize/prompt/request/close）备查。
//
// 官方参考：@deepseek-ai/dsh-sdk-client README（rc 线——锁版本）。
// 官方形态要点：
//   - launch 完全显式（command/args）——runtime 可执行由调用方命名
//   - 子进程懒启动、跨 run 复用、close() 强制（await using）
//   - provider 是 runtime 侧 cordis.yml 的职责（client 不组装请求）
//   - 无 mid-turn cancel（放弃 turn = 关闭 runtime）
//
// 已知前置（诚实标注）：
//   ① dsh runtime 构建（deepseek-harness monorepo 的 lib/bin.js +
//      cordis.yml）——DSH_RUNTIME_BIN/DSH_RUNTIME_ARGS env 参数化
//   ② provider 凭证（credentials 组——DeepSeek 官方 key 或兼容端点；
//      .env 无 DEEPSEEK_API_KEY——凭证策略由调用方持有）
// 前置就绪后 w28-dsh.sh 即插即用。
import http from "node:http";

const PORT = parseInt(process.argv[2] || "8040", 10);
const MARK = "DshSDK"; // 回复标记（路由证据）

// 官方调用点（runtime 就绪后启用）：
//   const { DeepSeekHarness } = await import("@deepseek-ai/dsh-sdk-client");
//   await using harness = new DeepSeekHarness({
//     launch: { command: process.env.DSH_RUNTIME_BIN || "node",
//               args: (process.env.DSH_RUNTIME_ARGS || "lib/bin.js cordis.yml").split(" ") },
//     provider: process.env.DSH_PROVIDER || "deepseek-official",
//     model: process.env.DSH_MODEL || "deepseek-v4-flash",
//     maxTokens: 49_152,
//   });
//   const result = await harness.run(prompt);
//   result.finalResponse → done 帧（Chronotope 账本自动落 usage）

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
  req.on("end", () => {
    res.writeHead(200, {
      "Content-Type": "text/event-stream",
      "Cache-Control": "no-cache",
    });
    sse(res, { type: "beat", seq: 0, payload: {} });
    // 骨架状态：runtime 未构建/凭证未配置——显式 error 帧（诚实）
    const ready = process.env.DSH_RUNTIME_BIN !== undefined;
    if (!ready) {
      sse(res, {
        type: "error",
        seq: 1,
        payload: {
          error:
            "dsh runtime 未构建（deepseek-harness monorepo 的 lib/bin.js + cordis.yml——DSH_RUNTIME_BIN/DSH_RUNTIME_ARGS 配置后启用 DeepSeekHarness）",
        },
      });
      return;
    }
    sse(res, {
      type: "done",
      seq: 1,
      payload: { final: `${MARK} 回复`, usage: {}, truncated: false },
    });
  });
});

server.listen(PORT, "0.0.0.0", () => {
  console.log(`dsh-sdk-harness listening on :${PORT}`);
});
