package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// stubHandler 是桩 harness（W1 D1 spike）：
//   - /once?label=x   返回本次请求序号（全局递增）；用于检测「已完成 step 是否被重放」
//   - /stream?dur=N   SSE 流：每秒 1 个 delta 事件、持续 N 秒，最后 done 帧——模拟分钟级 /runs 长流
//   - /stats          各路径命中计数（JSON）
type stubHandler struct {
	mu   sync.Mutex
	hits map[string]int
	next int
}

func newStubHandler() http.Handler {
	return &stubHandler{hits: make(map[string]int)}
}

func (s *stubHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if r.URL.RawQuery != "" {
		path += "?" + r.URL.RawQuery
	}
	s.mu.Lock()
	s.hits[path]++
	serial := s.next
	s.next++
	s.mu.Unlock()
	log.Printf("stub hit %s (serial=%d)", path, serial)

	switch r.URL.Path {
	case "/once":
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"serial":%d}`, serial)

	case "/stream":
		dur, _ := strconv.Atoi(r.URL.Query().Get("dur"))
		if dur <= 0 {
			dur = 1
		}
		fl, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		for i := 0; i < dur; i++ {
			fmt.Fprintf(w, "event: delta\ndata: %d\n\n", i)
			fl.Flush()
			time.Sleep(time.Second)
		}
		fmt.Fprint(w, "event: done\ndata: {}\n\n")
		fl.Flush()

	case "/stats":
		s.mu.Lock()
		defer s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		b, _ := json.Marshal(map[string]any{"hits": s.hits, "total": s.next})
		_, _ = w.Write(b)

	default:
		http.NotFound(w, r)
	}
}
