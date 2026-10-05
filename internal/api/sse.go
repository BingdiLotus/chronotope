package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/bingdilotus/chronotope/internal/store"
)

// sseEvent 是 SSE 时间轴的事件行（契约规范 §5 事件 schema 的投影）。
type sseEvent struct {
	SessionID string          `json:"session_id"`
	RunID     string          `json:"run_id"`
	Seq       int64           `json:"seq"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
	At        time.Time       `json:"at"`
}

// projectEvent 把 store 行投影为契约事件形状（getSession 与 SSE 共用，保证响应形状一致）。
func projectEvent(row store.EventRow) sseEvent {
	return sseEvent{
		SessionID: row.SessionID, RunID: row.RunID, Seq: row.Seq,
		Type: string(row.Type), Payload: row.Payload, At: row.At,
	}
}

// GET /sessions/{sessionID}/events?after=seq&limit —— SSE 时间轴（断线按 seq 续读）。
//
// 分发策略（落地方案 §5）：poller → hub 提示 (session_id, seq) → 此处回查 store 推送；
// 心跳 + 客户端 after=seq 对账兜底静默丢失；seq 允许 gap，订阅端必须容忍。
func (h *Handler) streamEvents(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "sessionID")
	if _, err := h.Store.GetSession(r.Context(), sessionID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, 404, "session not found")
			return
		}
		writeError(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	// after 缺省为 0（从头开始）；显式传参时校验（契约规范 §2：?after=seq 断线续读）
	after := int64(0)
	if raw := r.URL.Query().Get("after"); raw != "" {
		var err error
		if after, err = strconv.ParseInt(raw, 10, 64); err != nil || after < 0 {
			writeError(w, http.StatusBadRequest, 400, "after 必须为非负整数（契约规范 §2）")
			return
		}
	}
	limit := 100
	if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && l > 0 && l <= 500 {
		limit = l
	}

	fl, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, 500, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	writeRow := func(row store.EventRow) {
		ev := projectEvent(row)
		b, _ := json.Marshal(ev)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		fl.Flush()
	}

	// 断线重连的历史补读（after 之后的全部事件）
	rows, err := h.Store.ListEvents(r.Context(), sessionID, after, limit)
	if err != nil {
		h.Logger.Warn("sse backlog query failed", "err", err)
		return
	}
	lastSeq := after
	for _, row := range rows {
		writeRow(row)
		lastSeq = row.Seq
	}

	sub := h.Hub.Subscribe(r.Context(), sessionID, after)
	defer h.Hub.Unsubscribe(sub)

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			_, _ = fmt.Fprint(w, ": keepalive\n\n")
			fl.Flush()
		case <-sub.C():
			// 提示到达 → 回查（提示只带 seq，事件本体以 store 为准）
			fresh, err := h.Store.ListEvents(r.Context(), sessionID, lastSeq, 100)
			if err != nil {
				h.Logger.Warn("sse refetch failed", "err", err)
				continue
			}
			for _, row := range fresh {
				writeRow(row)
				lastSeq = row.Seq
			}
		}
	}
}
