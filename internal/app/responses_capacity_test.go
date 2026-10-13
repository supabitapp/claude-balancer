package app

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestHTTPCapacityRetryRetainsOwner(t *testing.T) {
	for _, code := range []string{"server_is_overloaded", "slow_down"} {
		for _, format := range []string{"HTTP rejection", "JSON response", "SSE response"} {
			t.Run(code+"/"+format, func(t *testing.T) {
				var attempts atomic.Int64
				accounts := make(chan string, 2)
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					accounts <- r.Header.Get("Chatgpt-Account-Id")
					io.Copy(io.Discard, r.Body)
					w.Header().Set("Retry-After", "7")
					if attempts.Add(1) != 1 {
						w.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprintf(w, "data: %s\n\ndata: %s\n\n", httpCreatedEvent, httpCompletedEvent)
						return
					}
					details := fmt.Sprintf(`{"code":%q,"type":"server_error","message":"capacity token-a","param":"model","extra":9007199254740993}`, code)
					response := fmt.Sprintf(`{"id":"resp-test","status":"failed","output":[],"error":%s}`, details)
					switch format {
					case "HTTP rejection":
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusServiceUnavailable)
						fmt.Fprintf(w, `{"error":%s}`, details)
					case "JSON response":
						w.Header().Set("Content-Type", "application/json")
						io.WriteString(w, response)
					case "SSE response":
						w.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprintf(w, "data: %s\n\ndata: {\"type\":\"response.failed\",\"response\":%s}\n\n", httpCreatedEvent, response)
					}
				}))
				defer upstream.Close()
				owner, fresh := testAccount("a", 0), testAccount("b", 20)
				srv, proxy := newWebSocketProxy(t, upstream.URL, []*Account{owner, fresh})
				srv.admission = newAdmissionGate(1)
				body := fmt.Sprintf(`{"model":"pool-model","input":[],"stream":%t}`, format == "SSE response")
				headers := http.Header{"Session-Id": {"capacity-session"}}
				first := postResponse(t, proxy.URL, body, headers)
				failure := readHTTPBody(t, first)
				status := http.StatusServiceUnavailable
				if format == "SSE response" {
					status = http.StatusOK
				}
				if first.StatusCode != status || first.Header.Get("Retry-After") != "7" || strings.Contains(failure, "token-a") {
					t.Fatalf("status=%d headers=%v body=%s", first.StatusCode, first.Header, failure)
				}
				for _, detail := range []string{`"code":"upstream_model_capacity"`, fmt.Sprintf(`"upstream_code":%q`, code), `"upstream_type":"server_error"`, `"param":"model"`, `"extra":9007199254740993`, "capacity [redacted]"} {
					if !strings.Contains(failure, detail) {
						t.Fatalf("missing %s in failure: %s", detail, failure)
					}
				}
				assertHTTPClean(t, srv)
				if attempts.Load() != 1 || <-accounts != "a" {
					t.Fatal("capacity failure replayed inference or selected the wrong account")
				}
				setTestAccountUsage(owner, 90)
				setTestAccountUsage(fresh, 0)
				fresh.RoutingMode = routingModePriority
				retry := postResponse(t, proxy.URL, body, headers)
				result := readHTTPBody(t, retry)
				if retry.StatusCode != http.StatusOK || !strings.Contains(result, `"status":"completed"`) || attempts.Load() != 2 || <-accounts != "a" {
					t.Fatalf("capacity retry lost its owner: status=%d attempts=%d body=%s", retry.StatusCode, attempts.Load(), result)
				}
				assertHTTPClean(t, srv)
			})
		}
	}
}
