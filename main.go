// Command index-translate-proxy serves an OpenAI-compatible bridge proxy for
// the free public Index-Translate API. It is a Go port of the behaviour of
// `python inference/llm/call_api.py --serve` from bilibili/Index-Translate.
//
// The proxy listens on 127.0.0.1:8080 by default (use --host 0.0.0.0 to expose
// it on the LAN) and forwards /chat/completions, /completions and /responses
// requests upstream, adding the CORS headers, User-Agent and thinking-off
// defaults that browser extensions such as Immersive Translate rely on.
package main

import (
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
	body = applyDefaults(body)

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

// applyDefaults mirrors the Python proxy: thinking stays disabled unless the
// client explicitly asks for it, and greedy decoding is pinned when the client
// omits the temperature. Bodies that are not JSON objects pass through untouched.
func applyDefaults(body []byte) []byte {
	if !utf8.Valid(body) {
		return body
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil || payload == nil {
		return body
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
		return body
	}
	return normalized
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
