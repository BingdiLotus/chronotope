package restate

import (
	"encoding/json"
	"fmt"
	"github.com/bingdilotus/chronotope/internal/core/memory"
	"log/slog"
	"strconv"
	"strings"

	restate "github.com/restatedev/sdk-go"

	"github.com/bingdilotus/chronotope/internal/core/event"
	"github.com/bingdilotus/chronotope/internal/core/runs"
)

// 分层记忆常量（边界语义设计 §7；MVP 无嵌入，检索按 topic 作用域 + recency）。
const (
	defaultTopic        = "default"
	memoryRetrieveK     = 5  // 组装注入的检索片段数（top-K）
	defaultConsolidateN = 40 // 消化触发阈值（消息数）
	summarizerPrompt    = "你是记忆消化器。将以下对话压缩成一段滚动摘要（保留用户目标、关键事实与未完成事项；不超过 200 字）。只输出摘要本身。"
)

// topicOf 取 run 的 topic 标签（默认 default；模型自动分类后置）。
func topicOf(in RunInput) string {
	if in.Topic != "" {
		return in.Topic
	}
	return defaultTopic
}

// thresholdOf 取消化阈值（0 用默认）。
func thresholdOf(deps *Deps) int {
	if deps.ConsolidateThreshold > 0 {
		return deps.ConsolidateThreshold
	}
	return defaultConsolidateN
}

// buildTranscript 把消息全量拼成消化转录（user/assistant 纯文本行）。
func buildTranscript(msgs []memory.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		var text string
		if err := json.Unmarshal(m.Content, &text); err != nil {
			continue
		}
		fmt.Fprintf(&b, "%s: %s\n", m.Role, text)
	}
	return b.String()
}

// recentUserMessages 取最近 limit 条消息中的 user 纯文本（条目抽取候选）。
func recentUserMessages(msgs []memory.Message, limit int) []string {
	if len(msgs) > limit {
		msgs = msgs[len(msgs)-limit:]
	}
	var out []string
	for _, m := range msgs {
		if m.Role != "user" {
			continue
		}
		var text string
		if err := json.Unmarshal(m.Content, &text); err != nil || strings.TrimSpace(text) == "" {
			continue
		}
		out = append(out, text)
	}
	return out
}

// consolidate 记忆消化工作流（run 结束后触发；边界语义设计 §7）：
// 转录 → 摘要 v_{n+1}（harness 摘要模式）→ 条目抽取（内容哈希去重 + 来源引用）
// → memory.consolidated 事件。journaled（同版本只消化一次，重放幂等）；
// 摘要失败不影响主流程（闭包内吞错，run 照常成功）。
func consolidate(ctx restate.Context, deps *Deps, sessionID, runID, topic string, emit *Emitter) error {
	threshold := thresholdOf(deps)
	msgs, err := deps.Store.ListMessages(ctx, sessionID, 1000)
	slog.Info("consolidate: check", "session", sessionID, "msgs", len(msgs), "threshold", threshold, "err", err)
	if err != nil || len(msgs) < threshold {
		return nil // 未达阈值或读取失败：跳过（失败不影响主流程）
	}

	version := 1
	if latest, err := deps.Store.LatestSummary(ctx, sessionID, topic); err == nil && latest != nil {
		version = latest.Version + 1
	}

	_, runErr := restate.Run(ctx, func(rc restate.RunContext) (any, error) {
		slog.Info("consolidate: closure enter", "session", sessionID, "version", version)
		// 摘要：harness 摘要模式（run_id 带 #consolidation 后缀 → FakeProvider 返回
		// 确定性摘要；真实模式用会话模型 + 摘要指令）。失败 → 跳过，不 fail run。
		res, err := deps.Harness.Call(rc, &runs.Request{
			Protocol:  runs.ProtocolVersion,
			RunID:     runID + "#consolidation",
			SessionID: sessionID,
			Step:      0,
			Model:     summarizerModelOf(deps, sessionID, ctx),
			Messages: []runs.Message{
				{Role: "system", Content: summarizerPrompt, Source: "trusted"},
				{Role: "user", Content: buildTranscript(msgs)},
			},
			Tools:          []runs.Tool{}, // 契约要求 list（nil 序列化为 null → harness 422）
			MaxTurns:       1,
			MaxOutputBytes: 524288,
		})
		summary := ""
		if err != nil {
			slog.Warn("consolidate: 摘要调用失败（跳过）", "err", err)
		} else if res != nil {
			summary = res.Final
		}
		if strings.TrimSpace(summary) == "" {
			return struct{}{}, nil // 摘要失败：跳过消化（下次 run 结束再试）
		}
		if _, err := deps.Store.CreateSummary(ctx, memory.Summary{
			SessionID: sessionID, Topic: topic, Version: version,
			Summary: summary, Diff: "full", CreatedByRun: runID,
		}); err != nil {
			return struct{}{}, nil // 落库失败同样跳过（幂等：同版本已存在即 false 成功）
		}
		// 条目抽取：最近 threshold 条中的用户消息（哈希去重 + 来源引用）
		added := 0
		for _, text := range recentUserMessages(msgs, threshold) {
			ok, _ := deps.Store.CreateMemoryItem(ctx, memory.MemoryItem{
				SessionID: sessionID, Topic: topic, Kind: "long_term",
				Content: text, SourceRunID: runID, Version: version,
			})
			if ok {
				added++
			}
		}
		_ = emit.Emit(ctx, sessionID, runID, 0, event.MemoryConsolidated, "memory", "", map[string]any{
			"topic": topic, "version": version, "items_added": added,
		})
		return struct{}{}, nil
	}, restate.WithName("consolidation:"+strconv.Itoa(version)))
	if runErr != nil {
		slog.Warn("consolidate: journal error（不影响主流程）", "err", runErr)
		return nil
	}
	slog.Info("consolidate: done", "session", sessionID, "version", version)
	return nil
}

// summarizerModelOf 摘要调用的模型：默认会话模型（FakeProvider 凭
// run_id 的 #consolidation 后缀返回确定性摘要）。
func summarizerModelOf(deps *Deps, sessionID string, ctx restate.Context) string {
	if state, err := deps.Sessions.GetState(ctx, sessionID); err == nil && state.AgentConfig.Model != "" {
		return state.AgentConfig.Model
	}
	return "claude-sonnet-4-6"
}
