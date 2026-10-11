// Command index-translate-proxy serves an OpenAI-compatible bridge proxy for
// the free public Index-Translate API. It is a Go port of the behaviour of
// `python inference/llm/call_api.py --serve` from bilibili/Index-Translate.
//
// The proxy listens on 127.0.0.1:8080 by default (use --host 0.0.0.0 to expose
// it on the LAN) and forwards /chat/completions, /completions and /responses
// requests upstream, adding the CORS headers, User-Agent and thinking-off
// defaults that browser extensions such as Immersive Translate rely on.
//
// Beyond the Python reference, /chat/completions always talks to the upstream
// in streaming mode: the free public API answers 504 when its origin stays
// silent for about ten seconds, and streaming keeps data flowing. Clients that
// did not ask for a stream receive the SSE stream reassembled into a regular
// response, and failed attempts are retried a few times before the last
// failure is handed to the client.
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

	// maxUpstreamAttempts bounds how many upstream requests one client request
	// may trigger: the initial attempt plus three retries.
	maxUpstreamAttempts = 4

	// defaultRetryDelay pauses between attempts so a flapping upstream is not
	// hammered with back-to-back requests.
	defaultRetryDelay = 500 * time.Millisecond

	// maxAssemblyBytes bounds how much of a streamed response is buffered
	// while reassembling a non-streaming response.
	maxAssemblyBytes = 16 << 20
)

// errAssemblyBudget marks a streamed response that grew past maxAssemblyBytes.
// A retry cannot shrink it, so the proxy reports the failure instead.
var errAssemblyBudget = errors.New("streamed response exceeds the assembly budget")

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
	apiBase    string
	client     *http.Client
	retryDelay time.Duration
}

func newProxy(apiBase string) *proxy {
	return &proxy{
		apiBase:    strings.TrimRight(apiBase, "/"),
		retryDelay: defaultRetryDelay,
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

	// /chat/completions runs through its own attempt loop: it always calls the
	// upstream in streaming mode and reassembles the stream for clients that
	// did not ask for one.
	if upstreamPath == "/chat/completions" && parsed {
		p.handleChatCompletion(w, r, payload)
		return
	}

	resp, err := p.postUpstream(r.Context(), upstreamPath, body)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(resp.Body)
		writeUpstreamError(w, resp.StatusCode, errBody)
		return
	}

	w.Header().Set("Access-Control-Allow-Origin", "*")
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "" {
		w.Header().Set("Cache-Control", cc)
	}
	w.WriteHeader(resp.StatusCode)
	streamBody(w, resp.Body)
}

// postUpstream sends a JSON body to an upstream path with the headers the
// upstream expects.
func (p *proxy) postUpstream(ctx context.Context, path string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.apiBase+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", upstreamAgent)
	return p.client.Do(req)
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

// handleChatCompletion serves /chat/completions through the attempt loop.
//
// Every upstream call is made in streaming mode: the free public API's gateway
// answers 504 when the origin stays silent for about ten seconds, and keeping
// data flowing avoids exactly that. Clients that asked for a stream receive the
// SSE bytes as they arrive; clients that did not receive the stream reassembled
// into a regular response body.
//
// A failed attempt is retried while attempts remain and the failure looks
// transient: connection errors, a response carrying no usable data, and the
// retryableStatus codes. Retries happen before any byte reaches the client,
// since a committed stream cannot be replayed. When every attempt fails, the
// client sees the last failure.
func (p *proxy) handleChatCompletion(w http.ResponseWriter, r *http.Request, payload map[string]any) {
	clientStream, _ := payload["stream"].(bool)

	upstreamBody, err := json.Marshal(streamRequestPayload(payload))
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, err)
		return
	}

	var (
		lastStatus int
		lastBody   []byte
		lastErr    error
	)
	for attempt := 1; attempt <= maxUpstreamAttempts; attempt++ {
		if attempt > 1 {
			if !sleepContext(r.Context(), p.retryDelay) {
				return // the client is gone; there is nobody left to answer
			}
			reason := "unknown failure"
			switch {
			case lastStatus != 0:
				reason = "HTTP " + strconv.Itoa(lastStatus)
			case lastErr != nil:
				reason = lastErr.Error()
			}
			logUpstreamEventf("/chat/completions attempt %d/%d failed (%s); retrying",
				attempt-1, maxUpstreamAttempts, reason)
		}

		resp, err := p.postUpstream(r.Context(), "/chat/completions", upstreamBody)
		if err != nil {
			lastStatus, lastBody, lastErr = 0, nil, err
			continue
		}
		if resp.StatusCode >= 400 {
			errBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			lastStatus, lastBody, lastErr = resp.StatusCode, errBody, nil
			if !retryableStatus(resp.StatusCode) {
				writeUpstreamError(w, resp.StatusCode, errBody)
				return
			}
			continue
		}
		if clientStream {
			if err := pipeStream(w, resp); err != nil {
				lastStatus, lastBody, lastErr = 0, nil, err
				continue
			}
			return
		}
		assembled, err := assembleChatCompletion(resp.Body)
		resp.Body.Close()
		if err == nil {
			writeAssembled(w, assembled)
			return
		}
		if errors.Is(err, errAssemblyBudget) {
			writeJSONError(w, http.StatusBadGateway, err)
			return
		}
		lastStatus, lastBody, lastErr = 0, nil, err
	}

	logUpstreamEventf("/chat/completions gave up after %d attempts", maxUpstreamAttempts)
	if lastStatus != 0 {
		writeUpstreamError(w, lastStatus, lastBody)
		return
	}
	writeJSONError(w, http.StatusBadGateway, lastErr)
}

// streamRequestPayload copies a client payload and switches the upstream call
// to streaming mode, requesting usage statistics in the final chunk unless the
// client sent stream_options of its own.
func streamRequestPayload(payload map[string]any) map[string]any {
	stream := make(map[string]any, len(payload)+2)
	for key, value := range payload {
		stream[key] = value
	}
	stream["stream"] = true
	if _, ok := stream["stream_options"]; !ok {
		stream["stream_options"] = map[string]any{"include_usage": true}
	}
	return stream
}

// pipeStream forwards a successful streaming response, waiting for the first
// body byte before committing the response so that an upstream which answers
// with headers and then closes without data is still reported as a failed
// attempt and can be retried.
func pipeStream(w http.ResponseWriter, resp *http.Response) error {
	defer resp.Body.Close()

	body := bufio.NewReader(resp.Body)
	if _, err := body.Peek(1); err != nil {
		return fmt.Errorf("upstream closed the stream before sending any data: %w", err)
	}

	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "" {
		w.Header().Set("Cache-Control", cc)
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(resp.StatusCode)
	streamBody(w, body)
	return nil
}

// retryableStatus reports whether an upstream status is worth another attempt:
// rate limiting and gateway failures are transient, client errors are not.
func retryableStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	}
	return false
}

// sleepContext waits for d unless the client disconnects first. It reports
// false when there is nobody left to answer.
func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// writeUpstreamError forwards an upstream error status and body with the CORS
// header, declaring JSON like the Python reference does.
func writeUpstreamError(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// logUpstreamEventf writes an operational note to stderr in the same shape as
// the request log, so retries show up in `docker logs`.
func logUpstreamEventf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "["+time.Now().Format("2006-01-02 15:04:05")+"] "+format+"\n", args...)
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
			return nil, errAssemblyBudget
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
