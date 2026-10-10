package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeUpstream records what the proxy forwarded and replies with a canned response.
type fakeUpstream struct {
	*httptest.Server
	mu          sync.Mutex
	path        string
	agent       string
	body        string
	status      int
	contentType string
	response    string
}

func newFakeUpstream(t *testing.T, status int, response string) *fakeUpstream {
	t.Helper()
	up := &fakeUpstream{status: status, contentType: "application/json", response: response}
	up.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		up.mu.Lock()
		up.path = r.URL.Path
		up.agent = r.Header.Get("User-Agent")
		up.body = string(body)
		up.mu.Unlock()
		w.Header().Set("Content-Type", up.contentType)
		w.WriteHeader(up.status)
		_, _ = io.WriteString(w, up.response)
	}))
	t.Cleanup(up.Close)
	return up
}

func (u *fakeUpstream) received() (path, agent, body string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.path, u.agent, u.body
}

func doRequest(p http.Handler, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

func TestChatCompletionsInjectsDefaults(t *testing.T) {
	const upstreamResponse = `{"id":"chatcmpl-1","choices":[{"message":{"content":"Hello, world."}}]}`
	up := newFakeUpstream(t, http.StatusOK, upstreamResponse)
	rec := doRequest(newProxy(up.URL), http.MethodPost, "/v1/chat/completions",
		`{"model":"Index-Translate-35B-A3B","messages":[{"role":"user","content":"你好"}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("CORS header = %q, want *", got)
	}
	if got := rec.Body.String(); got != upstreamResponse {
		t.Errorf("response body = %q, want upstream body", got)
	}

	path, agent, body := up.received()
	if path != "/chat/completions" {
		t.Errorf("upstream path = %q, want /chat/completions", path)
	}
	if agent != upstreamAgent {
		t.Errorf("upstream User-Agent = %q, want %q", agent, upstreamAgent)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("upstream body is not JSON: %v (%q)", err, body)
	}
	kwargs, _ := payload["chat_template_kwargs"].(map[string]any)
	if kwargs["enable_thinking"] != false {
		t.Errorf("enable_thinking = %v, want false", kwargs["enable_thinking"])
	}
	if temp, ok := payload["temperature"].(float64); !ok || temp != 0 {
		t.Errorf("temperature = %v, want 0", payload["temperature"])
	}
}

func TestClientSettingsArePreserved(t *testing.T) {
	up := newFakeUpstream(t, http.StatusOK, `{}`)
	doRequest(newProxy(up.URL), http.MethodPost, "/v1/chat/completions",
		`{"model":"m","temperature":0.7,"chat_template_kwargs":{"enable_thinking":true}}`)

	_, _, body := up.received()
	var payload map[string]any
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("upstream body is not JSON: %v (%q)", err, body)
	}
	if temp, _ := payload["temperature"].(float64); temp != 0.7 {
		t.Errorf("temperature = %v, want the client value 0.7", payload["temperature"])
	}
	kwargs, _ := payload["chat_template_kwargs"].(map[string]any)
	if kwargs["enable_thinking"] != true {
		t.Errorf("enable_thinking = %v, want the client value true", kwargs["enable_thinking"])
	}
}

func TestUpstreamPathMatchesRequest(t *testing.T) {
	tests := []struct{ request, want string }{
		{"/v1/chat/completions", "/chat/completions"},
		{"/v1/completions", "/completions"},
		{"/v1/responses", "/responses"},
	}
	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			up := newFakeUpstream(t, http.StatusOK, `{}`)
			doRequest(newProxy(up.URL), http.MethodPost, tc.request, `{}`)
			if path, _, _ := up.received(); path != tc.want {
				t.Errorf("upstream path = %q, want %q", path, tc.want)
			}
		})
	}
}

func TestNonJSONBodyPassesThrough(t *testing.T) {
	up := newFakeUpstream(t, http.StatusOK, `{}`)
	doRequest(newProxy(up.URL), http.MethodPost, "/v1/chat/completions", "not json")
	if _, _, body := up.received(); body != "not json" {
		t.Errorf("upstream body = %q, want unchanged", body)
	}
}

func TestModelsEndpoint(t *testing.T) {
	p := newProxy("http://127.0.0.1:1") // upstream is never reached here
	for _, path := range []string{"/v1/models", "/models"} {
		rec := doRequest(p, http.MethodGet, path, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200", path, rec.Code)
		}
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
			t.Errorf("GET %s CORS header = %q, want *", path, got)
		}
		var payload modelList
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("GET %s body is not JSON: %v", path, err)
		}
		if payload.Object != "list" || len(payload.Data) != 3 {
			t.Fatalf("GET %s payload = %+v, want a 3-entry list", path, payload)
		}
		if payload.Data[0].ID != "Index-Translate-35B-A3B" {
			t.Errorf("GET %s first model = %q, want Index-Translate-35B-A3B", path, payload.Data[0].ID)
		}
	}
}

func TestReadyResponse(t *testing.T) {
	rec := doRequest(newProxy("http://127.0.0.1:1"), http.MethodGet, "/", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "Index-Translate Proxy Ready\n" {
		t.Errorf("body = %q, want the ready text", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("CORS header = %q, want *", got)
	}
}

func TestOptionsPreflight(t *testing.T) {
	rec := doRequest(newProxy("http://127.0.0.1:1"), http.MethodOptions, "/v1/chat/completions", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	want := map[string]string{
		"Access-Control-Allow-Origin":  "*",
		"Access-Control-Allow-Methods": "GET, POST, OPTIONS",
		"Access-Control-Allow-Headers": "*",
		"Access-Control-Max-Age":       "86400",
	}
	for header, value := range want {
		if got := rec.Header().Get(header); got != value {
			t.Errorf("%s = %q, want %q", header, got, value)
		}
	}
}

func TestUnknownPostPathReturns404(t *testing.T) {
	rec := doRequest(newProxy("http://127.0.0.1:1"), http.MethodPost, "/v1/embeddings", `{}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestUpstreamErrorIsForwarded(t *testing.T) {
	const errBody = `{"error":{"message":"rate limited"}}`
	up := newFakeUpstream(t, http.StatusTooManyRequests, errBody)
	rec := doRequest(newProxy(up.URL), http.MethodPost, "/v1/chat/completions", `{}`)

	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", rec.Code)
	}
	if got := rec.Body.String(); got != errBody {
		t.Errorf("body = %q, want the upstream error body", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("CORS header = %q, want *", got)
	}
}

func TestUpstreamFailureReturnsBadGateway(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := up.URL
	up.Close() // the port now refuses connections

	rec := doRequest(newProxy(url), http.MethodPost, "/v1/chat/completions", `{}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	var payload map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("body is not JSON: %v (%q)", err, rec.Body.String())
	}
	if payload["error"] == "" {
		t.Error("body carries no error message")
	}
}

func TestStreamingResponsePassesThrough(t *testing.T) {
	const streamBody = "data: {\"choices\":[{\"delta\":{\"content\":\"你\"}}]}\n\ndata: [DONE]\n\n"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"你\"}}]}\n\n")
		flusher.Flush()
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(up.Close)

	rec := doRequest(newProxy(up.URL), http.MethodPost, "/v1/chat/completions", `{"stream":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != streamBody {
		t.Errorf("stream body = %q, want the upstream SSE payload", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
}
