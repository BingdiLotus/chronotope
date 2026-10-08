package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// TestWebhookSignature 期 5 §D：投递签名的生成与验证（接收方样例语义）。
func TestWebhookSignature(t *testing.T) {
	d := &Deliverer{SigningSecret: "test-secret"}
	body := []byte(`{"session_id":"s_1","type":"run.completed"}`)
	sig, ts := d.signWebhook("s_1", body)
	if sig == "" || ts == 0 {
		t.Fatal("签名应生成")
	}
	// 接收方验证（docs/Webhook-接入指南.md 的验证语义）：
	// 密钥 = HMAC(平台密钥, orgID)——org 由接收方按 session 归属约定获得
	mac := hmac.New(sha256.New, []byte("test-secret"))
	mac.Write([]byte(""))
	key := mac.Sum(nil)
	mac2 := hmac.New(sha256.New, key)
	mac2.Write(body)
	want := hex.EncodeToString(mac2.Sum(nil))
	if sig != want {
		t.Fatalf("签名不符: %s != %s", sig, want)
	}
}
