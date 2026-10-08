package execproto

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestStartProcessConnectFraming 官方云 Connect 帧编解码（协议形状单测——
// 真实 smoke 之外的回归：stdout/stderr base64 字段 + 终止帧 flags=2）。
func TestStartProcessConnectFraming(t *testing.T) {
	var events [][]byte
	ev := func(payload string) []byte { return []byte(payload) }
	events = append(events, ev(`{"event":{"start":{"pid":42}}}`))
	stdout := base64.StdEncoding.EncodeToString([]byte("hello\n"))
	events = append(events, ev(`{"event":{"data":{"stdout":"`+stdout+`"}}}`))
	events = append(events, ev(`{"event":{"end":{"exit_code":0,"exited":true,"status":"exit status 0"}}}`))

	mux := http.NewServeMux()
	mux.HandleFunc("/v2/sandboxes/sb_1/connect", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// domain 指向测试服务器（StartProcess 的 envd host——默认官方 host 会打真实网络）
		_, _ = w.Write([]byte(`{"sandboxID":"sb_1","envdAccessToken":"tok","domain":"http://` + r.Host + `"}`))
	})
	mux.HandleFunc("/process.Process/Start", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Access-Token") != "tok" || r.Header.Get("E2b-Sandbox-Id") != "sb_1" {
			t.Errorf("路由头缺失: %+v", r.Header)
		}
		// 请求帧校验（flags=0 + len + StartRequest）
		body := make([]byte, 5+1024)
		n, _ := r.Body.Read(body)
		if n < 5 || body[0] != 0 || int(binary.BigEndian.Uint32(body[1:5])) == 0 {
			t.Errorf("请求帧形状不符: % x", body[:n])
		}
		w.Header().Set("Content-Type", "application/connect+json")
		for _, payload := range events {
			frame := make([]byte, 5+len(payload))
			binary.BigEndian.PutUint32(frame[1:5], uint32(len(payload)))
			copy(frame[5:], payload)
			_, _ = w.Write(frame)
		}
		// 终止帧 flags=2
		term := make([]byte, 5)
		term[0] = 2
		_, _ = w.Write(term)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	api := NewHTTPE2BAPI(srv.URL, "key")
	stdout, stderr, exit, err := api.StartProcess(context.Background(), "sb_1", "echo hi")
	if err != nil || exit != 0 || stdout != "hello\n" || stderr != "" {
		t.Fatalf("start process: %q %q %d %v", stdout, stderr, exit, err)
	}
}

// TestStartProcessError 终止帧 error 对象 → 错误返回。
func TestStartProcessError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/sandboxes/sb_1/connect", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"sandboxID":"sb_1","envdAccessToken":"tok","domain":"http://` + r.Host + `"}`))
	})
	mux.HandleFunc("/process.Process/Start", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/connect+json")
		payload := []byte(`{"error":{"code":"invalid_argument","message":"bad cwd"}}`)
		frame := make([]byte, 5+len(payload))
		frame[0] = 2
		binary.BigEndian.PutUint32(frame[1:5], uint32(len(payload)))
		copy(frame[5:], payload)
		_, _ = w.Write(frame)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	_, _, _, err := NewHTTPE2BAPI(srv.URL, "key").StartProcess(context.Background(), "sb_1", "x")
	if err == nil {
		t.Fatal("error 帧应返回错误")
	}
}
