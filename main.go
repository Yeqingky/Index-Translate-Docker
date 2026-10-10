// Command index-translate-proxy serves an OpenAI-compatible bridge proxy for
// the free public Index-Translate API. It is a Go port of the behaviour of
// `python inference/llm/call_api.py --serve` from bilibili/Index-Translate.
//
// The proxy listens on 127.0.0.1:8080 by default (use --host 0.0.0.0 to expose
// it on the LAN) and forwards /chat/completions, /completions and /responses
// requests upstream, adding the CORS headers, User-Agent and thinking-off
// defaults that browser extensions such as Immersive Translate rely on.
//
// Beyond the Python reference, a non-streaming /chat/completions request that
// hits the upstream gateway timeout (504) is retried once in streaming mode
// and reassembled into a regular response: the free public API drops any
// request whose origin stays silent for about ten seconds, which streaming
// avoids by keeping data flowing.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	defaultAPIBase = "https://index-translate.bilibili.com/v1"
	upstreamAgent  = "Index-Translate-Client/1.0"

	// maxAssemblyBytes bounds how much of a retried SSE stream is buffered
	// while reassembling a non-streaming response.
	maxAssemblyBytes = 16 << 20
)

func main() {
	fs := flag.NewFlagSet("index-translate-proxy", flag.ExitOnError)
	serve := fs.Bool("serve", false, "start the local OpenAI-compatible bridge proxy for tools like Immersive Translate")
	host := fs.String("host", "127.0.0.1", "interface for --serve to bind (0.0.0.0 exposes it on the LAN)")
	port := fs.Int("port", 8080, "port for --serve to listen on")
	apiBase := fs.String("api-base", defaultAPIBase, "upstream Index-Translate API base URL")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: index-translate-proxy --serve [--host 127.0.0.1] [--port 8080] [--api-base URL]\n\n")
		fs.PrintDefaults()
	}
	_ = fs.Parse(os.Args[1:])

	if !*serve {
		fs.Usage()
		os.Exit(2)
	}
	if err := run(*host, *port, *apiBase); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(host string, port int, apiBase string) error {
	srv := &http.Server{
		Addr:              net.JoinHostPort(host, strconv.Itoa(port)),
		Handler:           logRequests(newProxy(apiBase)),
		ReadHeaderTimeout: 10 * time.Second,
	}

	printBanner(host, port, apiBase)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		fmt.Println("\nStopping proxy...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

func printBanner(host string, port int, apiBase string) {
	displayHost := host
	if host == "0.0.0.0" || host == "::" {
		displayHost = "127.0.0.1"
	}
	fmt.Println(strings.Repeat("=", 60))
	fmt.Printf("Index-Translate Local Proxy listening on %s:%d — http://%s:%d/v1\n", host, port, displayHost, port)
	fmt.Printf("Upstream API: %s\n\n", apiBase)
	fmt.Println("沉浸式翻译 (Immersive Translate) 配置指南:")
	fmt.Println("  1. 翻译服务选择: 自定义 (OpenAI 兼容)")
	fmt.Printf("  2. 接口地址 (API URL): http://%s:%d/v1\n", displayHost, port)
	fmt.Println("  3. 模型 (Model): Index-Translate-35B-A3B")
	fmt.Println("  4. API Key: 随意填写 (如 index)")
	fmt.Println(strings.Repeat("=", 60))
}

// proxy implements the OpenAI-compatible bridge.
type proxy struct {
	apiBase string
	client  *http.Client
}

func newProxy(apiBase string) *proxy {
	return &proxy{
		apiBase: strings.TrimRight(apiBase, "/"),
		client: &http.Client{
			// The Python reference applies a 120 s timeout to every socket
			// operation. Approximate it with dial and response-header timeouts
			// and leave the body unbounded so long SSE responses are not cut off.
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				DialContext:           (&net.Dialer{Timeout: 120 * time.Second}).DialContext,
				ResponseHeaderTimeout: 120 * time.Second,
			},
		},
	}
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodOptions:
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "*")
		h.Set("Access-Control-Max-Age", "86400")
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		p.handleGet(w, r)
	case http.MethodPost:
		p.handlePost(w, r)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

type modelEntry struct {
	ID     string `json:"id"`
	Object string `json:"object"`
}

type modelList struct {
	Object string       `json:"object"`
	Data   []modelEntry `json:"data"`
}

// modelsJSON mirrors the hard-coded model list of the Python proxy.
var modelsJSON = func() []byte {
	body, err := json.Marshal(modelList{Object: "list", Data: []modelEntry{
		{ID: "Index-Translate-35B-A3B", Object: "model"},
		{ID: "Index-Translate-2B", Object: "model"},
		{ID: "Index-Translate-9B", Object: "model"},
	}})
	if err != nil {
		panic(err)
	}
	return body
}()

func (p *proxy) handleGet(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/models" || r.URL.Path == "/models" {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Content-Length", strconv.Itoa(len(modelsJSON)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(modelsJSON)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "Index-Translate Proxy Ready\n")
}

func (p *proxy) handlePost(w http.ResponseWriter, r *http.Request) {
	// Forward to the same endpoint that was requested: a client posting to
	// /v1/completions must not be silently rewritten to /chat/completions.
	var upstreamPath string
	switch {
	case strings.HasSuffix(r.URL.Path, "/chat/completions"):
		upstreamPath = "/chat/completions"
	case strings.HasSuffix(r.URL.Path, "/completions"):
		upstreamPath = "/completions"
	case strings.HasSuffix(r.URL.Path, "/responses"):
		upstreamPath = "/responses"
	default:
		w.WriteHeader(http.StatusNotFound)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, err)
		return
	}
	body, payload, parsed := prepareRequestBody(body)
	streamRequested := false
	if parsed {
		streamRequested, _ = payload["stream"].(bool)
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, p.apiBase+upstreamPath, bytes.NewReader(body))
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", upstreamAgent)

	resp, err := p.client.Do(req)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, err)
		return
	}
	defer resp.Body.Close()

	// The upstream gateway returns 504 when the origin stays silent for about
	// ten seconds, which non-streaming requests exceeding that budget always
	// trigger. Retry those once in streaming mode, which keeps data flowing,
	// and reassemble the stream into a regular response.
	if resp.StatusCode == http.StatusGatewayTimeout && !streamRequested && parsed && upstreamPath == "/chat/completions" {
		errBody, _ := io.ReadAll(resp.Body)
		if assembled, ok := p.retryAsStream(r.Context(), payload, upstreamPath); ok {
			writeAssembled(w, assembled)
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(errBody)
		return
	}

	w.Header().Set("Access-Control-Allow-Origin", "*")

	if resp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(resp.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(errBody)
		return
	}

	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "" {
		w.Header().Set("Cache-Control", cc)
	}
	w.WriteHeader(resp.StatusCode)
	streamBody(w, resp.Body)
}

// prepareRequestBody applies the Python proxy defaults: thinking stays disabled
// unless the client explicitly asks for it, and greedy decoding is pinned when
// the client omits the temperature. It also returns the parsed payload so the
// caller can inspect and rewrite it; bodies that are not JSON objects pass
// through untouched.
func prepareRequestBody(body []byte) ([]byte, map[string]any, bool) {
	if !utf8.Valid(body) {
		return body, nil, false
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil || payload == nil {
		return body, nil, false
	}
	kwargs, ok := payload["chat_template_kwargs"].(map[string]any)
	if !ok {
		kwargs = map[string]any{}
		payload["chat_template_kwargs"] = kwargs
	}
	if _, ok := kwargs["enable_thinking"]; !ok {
		kwargs["enable_thinking"] = false
	}
	if temp, ok := payload["temperature"]; !ok || temp == nil {
		payload["temperature"] = 0.0
	}
	normalized, err := json.Marshal(payload)
	if err != nil {
		return body, nil, false
	}
	return normalized, payload, true
}

// retryAsStream repeats a request with streaming enabled and reassembles the
// SSE stream into a regular chat completion response.
func (p *proxy) retryAsStream(ctx context.Context, payload map[string]any, upstreamPath string) ([]byte, bool) {
	retry := make(map[string]any, len(payload)+2)
	for key, value := range payload {
		retry[key] = value
	}
	retry["stream"] = true
	if _, ok := retry["stream_options"]; !ok {
		retry["stream_options"] = map[string]any{"include_usage": true}
	}
	body, err := json.Marshal(retry)
	if err != nil {
		return nil, false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.apiBase+upstreamPath, bytes.NewReader(body))
	if err != nil {
		return nil, false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", upstreamAgent)

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, false
	}
	assembled, err := assembleChatCompletion(resp.Body)
	if err != nil {
		return nil, false
	}
	fmt.Fprintf(os.Stderr, "[%s] upstream 504 on %s; retried with stream and reassembled the response\n",
		time.Now().Format("2006-01-02 15:04:05"), upstreamPath)
	return assembled, true
}

type streamChunk struct {
	ID                string `json:"id"`
	Model             string `json:"model"`
	Created           int64  `json:"created"`
	SystemFingerprint string `json:"system_fingerprint"`
	Choices           []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage json.RawMessage `json:"usage"`
}

type assemblyMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type assemblyChoice struct {
	Index        int             `json:"index"`
	Message      assemblyMessage `json:"message"`
	FinishReason string          `json:"finish_reason"`
}

// assembleChatCompletion folds a chat.completions SSE stream into a single
// non-streaming response body.
func assembleChatCompletion(stream io.Reader) ([]byte, error) {
	scanner := bufio.NewScanner(stream)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	var content strings.Builder
	var meta streamChunk
	var finishReason string
	var usage json.RawMessage
	read := 0
	for scanner.Scan() {
		line := scanner.Text()
		read += len(line)
		if read > maxAssemblyBytes {
			return nil, errors.New("streamed response exceeds the assembly budget")
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := line[len("data: "):]
		if data == "[DONE]" {
			break
		}
		var chunk streamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if meta.ID == "" {
			meta = chunk
		}
		if len(chunk.Choices) > 0 {
			content.WriteString(chunk.Choices[0].Delta.Content)
			if reason := chunk.Choices[0].FinishReason; reason != nil {
				finishReason = *reason
			}
		}
		if len(chunk.Usage) > 0 && string(chunk.Usage) != "null" {
			usage = chunk.Usage
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if meta.ID == "" {
		return nil, errors.New("streamed response carried no chunks")
	}
	if finishReason == "" {
		finishReason = "stop"
	}

	assembled := struct {
		ID                string           `json:"id"`
		Object            string           `json:"object"`
		Created           int64            `json:"created"`
		Model             string           `json:"model"`
		SystemFingerprint string           `json:"system_fingerprint,omitempty"`
		Choices           []assemblyChoice `json:"choices"`
		Usage             json.RawMessage  `json:"usage,omitempty"`
	}{
		ID:                meta.ID,
		Object:            "chat.completion",
		Created:           meta.Created,
		Model:             meta.Model,
		SystemFingerprint: meta.SystemFingerprint,
		Choices: []assemblyChoice{{
			Index:        0,
			Message:      assemblyMessage{Role: "assistant", Content: content.String()},
			FinishReason: finishReason,
		}},
		Usage: usage,
	}
	return json.Marshal(assembled)
}

func writeAssembled(w http.ResponseWriter, body []byte) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func streamBody(w http.ResponseWriter, body io.Reader) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

func writeJSONError(w http.ResponseWriter, status int, err error) {
	body, marshalErr := json.Marshal(map[string]string{"error": err.Error()})
	if marshalErr != nil {
		body = []byte(`{"error":"unknown error"}`)
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// statusRecorder captures the response status for request logging and forwards
// Flush so streamed SSE responses keep flowing.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		fmt.Fprintf(os.Stderr, "[%s] %s %s %s %d\n", time.Now().Format("2006-01-02 15:04:05"), r.Method, r.URL.RequestURI(), r.Proto, rec.status)
	})
}
