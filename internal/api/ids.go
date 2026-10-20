package api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// genID 生成带前缀的资源 id（MVP：8 字节随机 hex）。
func genID(prefix string) string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand 不可用即系统级故障
	}
	return prefix + hex.EncodeToString(b)
}

// runIDFromIdempotency 实现幂等链第一跳（契约规范 §6）：
// Idempotency-Key(api) → run_id(worker)；同 (session_id, key) 恒得同一 run_id。
func runIDFromIdempotency(sessionID, idempotencyKey string) string {
	return resourceID("r_", sessionID, idempotencyKey)
}

func resourceID(prefix, namespace, key string) string {
	raw, _ := json.Marshal([]string{namespace, key})
	sum := sha256.Sum256(raw)
	return prefix + hex.EncodeToString(sum[:16])
}
