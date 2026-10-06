package api

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// GET /sessions/{sessionID}/export —— 标准 tar 导出（契约规范 §2；W8 交付）。
// 内容：manifest.json（会话/agent 元数据 + 计数）+ events.ndjson + messages.ndjson
// + memory.json（摘要/条目）+ usage.csv。真相与派生一并打包，交付物清单即 manifest。
func (h *Handler) exportSession(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "sessionID")
	sess, err := h.Store.GetSession(r.Context(), sessionID)
	if err != nil {
		writeError(w, http.StatusNotFound, 404, "session not found")
		return
	}
	events, err := h.Store.ListEvents(r.Context(), sessionID, 0, 10000)
	if err != nil {
		writeError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	messages, err := h.Store.ListMessages(r.Context(), sessionID, 10000)
	if err != nil {
		writeError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	summaries, _ := h.Store.ListSummaries(r.Context(), sessionID)
	items, _ := h.Store.ListMemoryItems(r.Context(), sessionID, "", 200)
	usage, _ := h.Store.ListUsage(r.Context(), sessionID)
	agent, err := h.Store.GetAgent(r.Context(), sess.AgentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}

	manifest := map[string]any{
		"session_id": sessionID, "agent_id": sess.AgentID, "status": string(sess.Status),
		"model": agent.Config.Model, "instructions": agent.Config.Instructions,
		"exported_at": time.Now().UTC().Format(time.RFC3339),
		"counts": map[string]int{
			"events": len(events), "messages": len(messages),
			"summaries": len(summaries), "memory_items": len(items), "usage_buckets": len(usage),
		},
	}

	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="chronotope-%s.tar.gz"`, sessionID))
	gz := gzip.NewWriter(w)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	writeEntry := func(name string, data []byte) {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), ModTime: time.Now()})
		_, _ = tw.Write(data)
	}
	mb, _ := json.MarshalIndent(manifest, "", "  ")
	writeEntry("manifest.json", mb)

	var eb strings.Builder
	for _, ev := range events {
		b, _ := json.Marshal(ev)
		eb.Write(b)
		eb.WriteByte('\n')
	}
	writeEntry("events.ndjson", []byte(eb.String()))

	var mbuf strings.Builder
	for _, m := range messages {
		b, _ := json.Marshal(m)
		mbuf.Write(b)
		mbuf.WriteByte('\n')
	}
	writeEntry("messages.ndjson", []byte(mbuf.String()))

	memJSON, _ := json.MarshalIndent(map[string]any{"summaries": summaries, "items": items}, "", "  ")
	writeEntry("memory.json", memJSON)

	var ub strings.Builder
	ub.WriteString("bucket,active_seconds,tokens_in,tokens_out,compute_seconds\n")
	for _, u := range usage {
		ub.WriteString(fmt.Sprintf("%s,%.3f,%d,%d,%.3f\n",
			u.Bucket.UTC().Format(time.RFC3339), u.ActiveSeconds, u.TokensIn, u.TokensOut, u.ComputeSeconds))
	}
	writeEntry("usage.csv", []byte(ub.String()))
}
