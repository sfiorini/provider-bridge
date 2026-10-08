//go:build e2e

// Package e2e_test contains end-to-end key-rotation tests: full-server
// boots (inbound adapter → core → provider adapter → rotating upstream
// client) against httptest mock upstreams that dispatch on the API-key
// header, proving comma-separated api_key rotation on HTTP 429 across the
// chat streaming path, the anthropic non-streaming path, and a full
// restart boundary backed by a file-based SQLite data dir.
package e2e_test

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"providerbridge/internal/config"
	"providerbridge/internal/db"
	"providerbridge/internal/format"
	"providerbridge/internal/protocol/anthropic"
	"providerbridge/internal/protocol/chat"
	"providerbridge/internal/service/provider"
	"providerbridge/internal/service/runtime"
	"providerbridge/internal/service/server"
	"providerbridge/internal/service/store"

	_ "modernc.org/sqlite"
)

// ============================================================================
// Mock upstreams (key-dispatching)
// ============================================================================

// krAuthLog records the API-key credential of every upstream request in
// arrival order so tests can assert the exact key sequence.
type krAuthLog struct {
	mu   sync.Mutex
	auth []string
}

func (l *krAuthLog) add(credential string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.auth = append(l.auth, credential)
}

func (l *krAuthLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.auth))
	copy(out, l.auth)
	return out
}

// krChatMock starts an OpenAI-chat upstream that answers "Bearer k1" with
// HTTP 429 and any other Bearer key (the tests use "Bearer k2") with a
// streaming SSE completion: one text chunk, one finish chunk, and the
// terminal DONE marker.
func krChatMock(t *testing.T, log *krAuthLog) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		log.add(auth)
		if auth == "Bearer k1" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error": {"message": "rate limited", "type": "rate_limit_error"}}`)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		fmt.Fprint(w, "data: "+`{"id":"kr-1","object":"chat.completion.chunk","created":1,"model":"upstream-chat","choices":[{"index":0,"delta":{"role":"assistant","content":"PONG"},"finish_reason":null}]}`+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		fmt.Fprint(w, "data: "+`{"id":"kr-2","object":"chat.completion.chunk","created":1,"model":"upstream-chat","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		fmt.Fprint(w, "data: "+"["+"DONE"+"]"+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// krAnthropicMock starts an anthropic upstream that answers "x-api-key: k1"
// with HTTP 429 and any other key (the tests use "k2") with a full
// non-streaming MessageResponse carrying one text content block.
func krAnthropicMock(t *testing.T, log *krAuthLog) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("x-api-key")
		log.add(key)
		if key == "k1" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"type": "error", "error": {"type": "rate_limit_error", "message": "rate limited"}}`)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"msg_kr","type":"message","role":"assistant","model":"upstream-anthropic","content":[{"type":"text","text":"PONG"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// ============================================================================
// Bridge boot (full server, provider defs with comma api_key)
// ============================================================================

// krBootBridge builds a full Server around the given config. When rotStore
// is non-nil it is attached to the provider manager (and runtime), mirroring
// app boot: persisted active key indexes are loaded and clamped at startup.
func krBootBridge(t *testing.T, cfg config.Config, rotStore store.ConfigStore) http.Handler {
	t.Helper()

	providerDefs := krProviderDefs(cfg)
	modelRoutes := krModelRoutes(cfg)
	providerMgr, err := provider.NewProviderManager(providerDefs, modelRoutes)
	if err != nil {
		t.Fatalf("failed to create provider manager: %v", err)
	}

	coreHooks := format.CorePluginHooks{}.WithDefaults()
	adapterReg := format.NewRegistry()
	for _, def := range cfg.ProviderDefs {
		switch def.Protocol {
		case config.ProtocolOpenAIChat:
			chatClientAdapter := chat.NewChatClientAdapter(coreHooks)
			_ = adapterReg.RegisterClient(chatClientAdapter)
			_ = adapterReg.RegisterClientStream(chatClientAdapter)
			chatProviderAdapter := chat.NewChatProviderAdapter(2048, nil, coreHooks)
			_ = adapterReg.RegisterProvider(chatProviderAdapter)
			_ = adapterReg.RegisterProviderStream(chatProviderAdapter)
		case config.ProtocolAnthropic:
			anthClientAdapter := anthropic.NewAnthropicClientAdapter(coreHooks)
			_ = adapterReg.RegisterClient(anthClientAdapter)
			_ = adapterReg.RegisterClientStream(anthClientAdapter)
			anthProviderAdapter := anthropic.NewAnthropicProviderAdapter(2048, &noopCacheManager{}, coreHooks)
			_ = adapterReg.RegisterProvider(anthProviderAdapter)
			_ = adapterReg.RegisterProviderStream(anthProviderAdapter)
		}
	}

	rt := runtime.NewRuntime(cfg, providerMgr, nil)
	if rotStore != nil {
		providerMgr.SetKeyRotationStore(rotStore)
		rt.SetKeyRotationStore(rotStore)
	}

	serverCfg := config.ServerFromGlobalConfig(&cfg)
	return server.New(server.Config{
		ProviderMgr:     providerMgr,
		AdapterRegistry: adapterReg,
		AppConfig:       serverCfg,
		ServerCfg:       serverCfg,
		Runtime:         rt,
	})
}

// krProviderDefs converts config.ProviderDefs into provider.ProviderConfig.
func krProviderDefs(cfg config.Config) map[string]provider.ProviderConfig {
	defs := make(map[string]provider.ProviderConfig, len(cfg.ProviderDefs))
	for key, def := range cfg.ProviderDefs {
		modelNames := make([]string, 0, len(def.Models))
		models := make(map[string]provider.ModelMeta, len(def.Models))
		for name, meta := range def.Models {
			modelNames = append(modelNames, name)
			models[name] = provider.ModelMeta(meta)
		}
		defs[key] = provider.ProviderConfig{
			BaseURL:          def.BaseURL,
			APIKey:           def.APIKey,
			Version:          def.Version,
			UserAgent:        def.UserAgent,
			Protocol:         def.Protocol,
			WebSearchSupport: string(def.WebSearchSupport),
			ModelNames:       modelNames,
			Models:           models,
			Offers:           def.Offers,
		}
	}
	return defs
}

// krModelRoutes converts config.Routes into provider.ModelRoute.
func krModelRoutes(cfg config.Config) map[string]provider.ModelRoute {
	routes := make(map[string]provider.ModelRoute, len(cfg.Routes))
	for alias, route := range cfg.Routes {
		routes[alias] = provider.ModelRoute{
			Provider: route.Provider,
			Name:     route.Model,
		}
	}
	return routes
}

// ============================================================================
// File-based SQLite key-rotation store (restart test data dir)
// ============================================================================

// No existing e2e harness boots with a data dir: internal/e2e boots adapter
// tiers only, and internal/service/e2e full-server tests boot with in-memory
// configs. The restart test therefore brings its own file-based SQLite
// store (the db_sqlite persistence mode the app uses), so both bridge boots
// share a real on-disk data dir.

// krFileStore is a minimal db.Store over a file-based SQLite database with
// the config_store tables (real names prefixed "config_store_", mirroring
// db registry table isolation).
type krFileStore struct {
	db     *sql.DB
	tables map[string]string
}

func krOpenFileStore(t *testing.T, path string) *krFileStore {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open sqlite %s: %v", path, err)
	}
	database.SetMaxOpenConns(1)
	if _, err := database.Exec("PRAGMA busy_timeout=5000"); err != nil {
		database.Close()
		t.Fatalf("set busy_timeout: %v", err)
	}

	consumer := store.NewConfigStoreConsumer(slog.Default())
	tables := make(map[string]string)
	for _, tbl := range consumer.Tables() {
		realName := consumer.Name() + "_" + tbl.Name
		ddl := strings.ReplaceAll(tbl.Schema, "{{table}}", realName)
		if _, err := database.Exec(ddl); err != nil {
			database.Close()
			t.Fatalf("create table %q: %v", realName, err)
		}
		tables[tbl.Name] = realName
	}
	return &krFileStore{db: database, tables: tables}
}

func (s *krFileStore) ConsumerName() string { return "config_store" }
func (s *krFileStore) Dialect() db.Dialect  { return db.DialectSQLite }
func (s *krFileStore) Table(localName string) (string, error) {
	name, ok := s.tables[localName]
	if !ok {
		return "", db.ErrTableNotRegistered
	}
	return name, nil
}
func (s *krFileStore) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return s.db.ExecContext(ctx, query, args...)
}
func (s *krFileStore) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return s.db.QueryContext(ctx, query, args...)
}
func (s *krFileStore) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return s.db.QueryRowContext(ctx, query, args...)
}
func (s *krFileStore) WithTx(ctx context.Context, fn func(db.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	wrapped := &krTx{tx: tx, tables: s.tables}
	if err := fn(wrapped); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
func (s *krFileStore) Close() error { return s.db.Close() }

// krTx adapts *sql.Tx to db.Tx with the same table-name isolation.
type krTx struct {
	tx     *sql.Tx
	tables map[string]string
}

func (t *krTx) Table(localName string) (string, error) {
	name, ok := t.tables[localName]
	if !ok {
		return "", db.ErrTableNotRegistered
	}
	return name, nil
}
func (t *krTx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return t.tx.ExecContext(ctx, query, args...)
}
func (t *krTx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return t.tx.QueryContext(ctx, query, args...)
}
func (t *krTx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return t.tx.QueryRowContext(ctx, query, args...)
}

// ============================================================================
// Inbound request helpers
// ============================================================================

// krPostChatStream POSTs a streaming /v1/chat/completions request to the
// booted bridge and returns the HTTP status plus every SSE data payload.
func krPostChatStream(t *testing.T, handler http.Handler, payload map[string]any) (int, []string) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload error = %v", err)
	}
	target := httptest.NewServer(handler)
	t.Cleanup(target.Close)
	resp, err := http.Post(target.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post chat completions error = %v", err)
	}
	defer resp.Body.Close()

	var events []string
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		events = append(events, strings.TrimPrefix(line, "data: "))
	}
	return resp.StatusCode, events
}

// krPostMessages POSTs a non-streaming /v1/messages request and returns the
// HTTP status plus the raw JSON body.
func krPostMessages(t *testing.T, handler http.Handler, payload map[string]any) (int, string) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload error = %v", err)
	}
	target := httptest.NewServer(handler)
	t.Cleanup(target.Close)
	resp, err := http.Post(target.URL+"/v1/messages", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post messages error = %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read messages body error = %v", err)
	}
	return resp.StatusCode, string(raw)
}

// ============================================================================
// Tests
// ============================================================================

// TestKeyRotationChatE2E streams a chat completion through the full bridge
// against a mock upstream where the first key (k1) is rate limited with HTTP
// 429. The bridge must transparently retry the identical request with the
// next key (k2), complete the stream with non-empty content, and leave
// exactly two upstream requests.
func TestKeyRotationChatE2E(t *testing.T) {
	log := &krAuthLog{}
	upstream := krChatMock(t, log)

	cfg := config.Config{
		Routes: map[string]config.RouteEntry{
			"kr-chat-model": {Provider: "kr-chat-main", Model: "upstream-chat"},
		},
		ProviderDefs: map[string]config.ProviderDef{
			"kr-chat-main": {
				BaseURL:  upstream.URL,
				APIKey:   "k1,k2",
				Protocol: config.ProtocolOpenAIChat,
				Models: map[string]config.ModelMeta{
					"upstream-chat": {InputModalities: []string{"text"}},
				},
			},
		},
	}

	handler := krBootBridge(t, cfg, nil)

	status, events := krPostChatStream(t, handler, map[string]any{
		"model":  "kr-chat-model",
		"stream": true,
		"messages": []any{
			map[string]any{"role": "user", "content": "Say PONG"},
		},
	})

	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}

	// The streamed content must be non-empty (the k2 SSE text chunk survived
	// the wire end to end).
	var content strings.Builder
	for _, event := range events {
		var chunk struct {
			Object  string `json:"object"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(event), &chunk); err != nil {
			continue
		}
		if chunk.Object != "chat.completion.chunk" {
			continue
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" {
				content.WriteString(choice.Delta.Content)
			}
		}
	}
	if content.Len() == 0 {
		t.Fatalf("streamed content is empty; events = %v", events)
	}

	// Exactly two upstream requests: k1 (429) then k2 (success).
	auth := log.all()
	if len(auth) != 2 {
		t.Fatalf("upstream request count = %d (auth = %v), want exactly 2", len(auth), auth)
	}
	if auth[0] != "Bearer k1" {
		t.Errorf("first upstream request Authorization = %q, want %q", auth[0], "Bearer k1")
	}
	if auth[1] != "Bearer k2" {
		t.Errorf("second upstream request Authorization = %q, want %q", auth[1], "Bearer k2")
	}
}

// TestKeyRotationAnthropicE2E sends a non-streaming /v1/messages request
// through the full bridge against a mock anthropic upstream where the first
// key (k1) is rate limited with HTTP 429. The bridge must retry with the
// next key (k2), return a response carrying a content block, and leave
// exactly two upstream requests.
func TestKeyRotationAnthropicE2E(t *testing.T) {
	log := &krAuthLog{}
	upstream := krAnthropicMock(t, log)

	cfg := config.Config{
		Routes: map[string]config.RouteEntry{
			"kr-anthropic-model": {Provider: "kr-anthropic-main", Model: "upstream-anthropic"},
		},
		ProviderDefs: map[string]config.ProviderDef{
			"kr-anthropic-main": {
				BaseURL:  upstream.URL,
				APIKey:   "k1,k2",
				Version:  "2023-06-01",
				Protocol: config.ProtocolAnthropic,
				Models: map[string]config.ModelMeta{
					"upstream-anthropic": {InputModalities: []string{"text"}},
				},
			},
		},
	}

	handler := krBootBridge(t, cfg, nil)

	status, raw := krPostMessages(t, handler, map[string]any{
		"model":      "kr-anthropic-model",
		"max_tokens": 64,
		"messages": []any{
			map[string]any{"role": "user", "content": "Say PONG"},
		},
	})

	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", status, http.StatusOK, raw)
	}

	// The non-stream response must carry a content block (the k2
	// MessageResponse survived the wire end to end).
	var resp struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatalf("response is not JSON: %v; body = %s", err, raw)
	}
	if len(resp.Content) == 0 {
		t.Fatalf("response has no content block; body = %s", raw)
	}
	if resp.Content[0].Type != "text" || resp.Content[0].Text == "" {
		t.Errorf("content[0] = {type: %q, text: %q}, want a non-empty text block", resp.Content[0].Type, resp.Content[0].Text)
	}

	// Exactly two upstream requests: k1 (429) then k2 (success).
	keys := log.all()
	if len(keys) != 2 {
		t.Fatalf("upstream request count = %d (keys = %v), want exactly 2", len(keys), keys)
	}
	if keys[0] != "k1" {
		t.Errorf("first upstream request x-api-key = %q, want %q", keys[0], "k1")
	}
	if keys[1] != "k2" {
		t.Errorf("second upstream request x-api-key = %q, want %q", keys[1], "k2")
	}
}

// TestKeyRotationPersistsAcrossRestart proves the advanced key index
// survives a bridge restart. The first bridge rotates k1 (429) → k2 (200);
// after the async persist lands, a NEW bridge is booted against the same
// file-based SQLite data dir and its very first upstream request must go
// straight to k2.
func TestKeyRotationPersistsAcrossRestart(t *testing.T) {
	log := &krAuthLog{}
	upstream := krChatMock(t, log)

	dataDir := filepath.Join(t.TempDir(), "data")
	dbPath := filepath.Join(dataDir, "provider-bridge.db")

	cfg := config.Config{
		Routes: map[string]config.RouteEntry{
			"kr-chat-model": {Provider: "kr-chat-main", Model: "upstream-chat"},
		},
		ProviderDefs: map[string]config.ProviderDef{
			"kr-chat-main": {
				BaseURL:  upstream.URL,
				APIKey:   "k1,k2",
				Protocol: config.ProtocolOpenAIChat,
				Models: map[string]config.ModelMeta{
					"upstream-chat": {InputModalities: []string{"text"}},
				},
			},
		},
	}

	chatReq := map[string]any{
		"model":  "kr-chat-model",
		"stream": true,
		"messages": []any{
			map[string]any{"role": "user", "content": "Say PONG"},
		},
	}

	// --- Bridge A: rotate k1 → k2 on 429. ---
	fsA := krOpenFileStore(t, dbPath)
	rotStoreA := store.NewSQLiteStore(fsA, slog.Default())
	handlerA := krBootBridge(t, cfg, rotStoreA)

	status, events := krPostChatStream(t, handlerA, chatReq)
	if status != http.StatusOK {
		t.Fatalf("bridge A status = %d, want %d", status, http.StatusOK)
	}
	if len(events) == 0 {
		t.Fatal("bridge A stream produced no events")
	}

	// The index persist is asynchronous and best-effort: wait for it to
	// land before "restarting".
	deadline := time.Now().Add(5 * time.Second)
	for {
		indexes, err := rotStoreA.LoadProviderKeyIndexes(context.Background())
		if err != nil {
			t.Fatalf("LoadProviderKeyIndexes: %v", err)
		}
		if idx, ok := indexes["kr-chat-main"]; ok && idx == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("persisted key index never advanced to 1; indexes = %v", indexes)
		}
		time.Sleep(25 * time.Millisecond)
	}

	// Stop bridge A (close its data dir).
	if err := fsA.Close(); err != nil {
		t.Fatalf("close bridge A data dir: %v", err)
	}

	// --- Bridge B: NEW boot against the SAME data dir. ---
	fsB := krOpenFileStore(t, dbPath)
	rotStoreB := store.NewSQLiteStore(fsB, slog.Default())
	handlerB := krBootBridge(t, cfg, rotStoreB)

	authBefore := len(log.all())
	status, events = krPostChatStream(t, handlerB, chatReq)
	if status != http.StatusOK {
		t.Fatalf("bridge B status = %d, want %d", status, http.StatusOK)
	}
	if len(events) == 0 {
		t.Fatal("bridge B stream produced no events")
	}

	// The new bridge's FIRST upstream request must use the persisted active
	// key k2 (no 429 round-trip through k1).
	auth := log.all()
	newAuth := auth[authBefore:]
	if len(newAuth) == 0 {
		t.Fatal("bridge B made no upstream requests")
	}
	if newAuth[0] != "Bearer k2" {
		t.Fatalf("bridge B first upstream Authorization = %q, want %q (all = %v)", newAuth[0], "Bearer k2", auth)
	}
}
