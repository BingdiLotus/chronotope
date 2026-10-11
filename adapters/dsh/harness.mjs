// dsh 官方 SDK 适配器（批 4——真实实现：headless --json 的官方机器可读形态）。
//
// 经 /runs 协议接 Chronotope；spawn dsh headless CLI（官方 --json 的
// ndjson run events）→ 帧映射（text/final/step_end+usage → delta/done）。
// 官方参考：@deepseek-ai/dsh-sdk-client README + docs/user/guide/providers.md。
//
// 用户指正（2026-10-11）：dsh 支持 openai 格式接口——Custom model API
// （apiKeyEnv + openai-completions + baseURL 指 poke2api）——无需
// DEEPSEEK key；装配验证实证：--dump-config 的 composed 树含 provider +
// headless 真实 run（gpt-6.1-sol 经 poke2api——reasoning+回答完整）。
//
// 三铁律：无状态（每次 run spawn 新 headless 进程——dsh 的 session 不
// 持久化到 Chronotope）；usage → 账本（step_end 的 usage 映射）。
import http from "node:http";
import { spawn } from "node:child_process";

const PORT = parseInt(process.argv[2] || "8040", 10);
const MARK = "DshSDK"; // 回复标记（路由证据）

// spawn headless --json → ndjson events → final + usage
async function runDsh(prompt) {
  const bin = process.env.DSH_RUNTIME_BIN || "node";
  const args = (process.env.DSH_RUNTIME_ARGS ||
    "/tmp/dsh/apps/cli/lib/bin.js --profile headless --patch <cordis> --json").split(" ");
  const cordis = process.env.DSH_CORDIS ||
    new URL("./cordis.yml", import.meta.url).pathname;
  const argv = args.map((a) => (a === "<cordis>" ? cordis : a));
  argv.push(prompt);
  return new Promise((resolve) => {
    const child = spawn(bin, argv, { env: process.env });
    let final = "", usage = {}, buf = "";
    child.stdout.on("data", (c) => {
      buf += c;
      let i;
      while ((i = buf.indexOf("\n")) >= 0) {
        const line = buf.slice(0, i); buf = buf.slice(i + 1);
        if (!line) continue;
        try {
          const ev = JSON.parse(line);
          if (ev.type === "text") final += ev.text || "";
          if (ev.type === "final") final = ev.text || final;
          if (ev.type === "status" && ev.phase === "step_end" && ev.usage) {
            usage = { tokens_in: ev.usage.inputTokens || 0, tokens_out: ev.usage.outputTokens || 0 };
          }
        } catch {}
      }
    });
    child.stderr.on("data", () => {});
    child.on("close", () => resolve({ final, usage }));
  });
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
    const prompt = (body.messages || [])
      .filter((m) => m.role === "user")
      .map((m) => m.content)
      .join("\n");
    try {
      const { final, usage } = await runDsh(prompt);
      if (!final) {
        sse(res, { type: "error", seq: 1, payload: { error: "dsh run 无 final（runtime 或 provider 未就绪）" } });
        res.end();
        return;
      }
      sse(res, { type: "delta", seq: 1, payload: { text: final } });
      sse(res, { type: "done", seq: 2, payload: { final: `${final} [${MARK}]`.trim(), usage, truncated: false } });
      res.end();
    } catch (e) {
      sse(res, { type: "error", seq: 1, payload: { error: String(e) } });
      res.end();
    }
  });
});

server.listen(PORT, "0.0.0.0", () => {
  console.log(`dsh-sdk-harness listening on :${PORT}`);
});
