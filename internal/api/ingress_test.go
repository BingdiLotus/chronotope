package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIngressSendRequiresDurableAcceptance(t *testing.T) {
	for _, status := range []int{http.StatusAccepted, http.StatusOK, http.StatusConflict, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var path string
			var command map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				path = r.Method + " " + r.URL.Path
				if err := json.NewDecoder(r.Body).Decode(&command); err != nil {
					t.Error(err)
				}
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"invocationId":"accepted-invocation"}`))
			}))
			defer server.Close()
			ingress := NewHTTPIngress(server.URL)
			err := ingress.Send(context.Background(), "/run_workflow/r_stable/run", map[string]any{"session_id": "s", "input": "original"})
			if (err == nil) != (status == http.StatusAccepted) {
				t.Fatalf("status %d acceptance result: %v", status, err)
			}
			if path != "POST /run_workflow/r_stable/run/send" || command["session_id"] != "s" || command["input"] != "original" {
				t.Fatalf("wrong asynchronous submission: %s %v", path, command)
			}
		})
	}
}

func TestIngressCallsRespectCallerCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("canceled request must not be sent") }))
	defer server.Close()
	ingress := NewHTTPIngress(server.URL)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, err := range []error{
		ingress.Send(ctx, "/run_workflow/r/run", map[string]any{"input": "x"}),
		ingress.Call(ctx, "/session_object/s/Create", http.MethodPost, nil, nil),
	} {
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("must respect canceled caller: %v", err)
		}
	}
}
