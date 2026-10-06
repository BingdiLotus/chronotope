package api

import (
	"net/http"
	"strings"
)

// corsMiddleware 控制台跨源（web 是独立源直连 API；SSE 事件流不能经代理缓冲——
// P2-2 实证 next dev rewrite 缓冲 SSE 导致时间轴不流动）。
// 允许源经 API_CORS_ORIGINS 配置（逗号分隔，默认 http://localhost:3000）。
// CorsMiddleware 导出（cmd/api 挂载）。
func CorsMiddleware(allowedOrigins string) func(http.Handler) http.Handler {
	if strings.TrimSpace(allowedOrigins) == "" {
		allowedOrigins = "http://localhost:3000" // 控制台默认源
	}
	allowed := map[string]bool{}
	for _, o := range strings.Split(allowedOrigins, ",") {
		if o = strings.TrimSpace(o); o != "" {
			allowed[o] = true
		}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin != "" && allowed[origin] {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Vary", "Origin")
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Idempotency-Key")
				w.Header().Set("Access-Control-Expose-Headers", "Retry-After")
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
