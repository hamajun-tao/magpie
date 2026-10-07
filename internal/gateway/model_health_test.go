package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

func TestModelHealthGatewayFiltersEveryRoute(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	var mu sync.Mutex
	seen := map[string]int{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" {
			w.Write([]byte(`{"data":[{"id":"qwen"},{"id":"bad"}]}`))
			return
		}
		var in struct{ Model string }
		json.NewDecoder(r.Body).Decode(&in)
		mu.Lock()
		seen[r.Header.Get("Authorization")+"/"+in.Model]++
		mu.Unlock()
		if in.Model == "bad" || r.Header.Get("Authorization") == "Bearer bad-key" {
			w.WriteHeader(404)
			w.Write([]byte(`{"error":{"message":"no channel"}}`))
			return
		}
		w.Write([]byte(`{"id":"healthy-key","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer up.Close()
	p := provider.Provider{ID: "relay", Chat: up.URL + "/v1", Key: "bad-key", Keys: []provider.KeyAccount{{Key: "good-key"}}, Models: []string{"qwen", "bad"}}
	if err := provider.Save(p); err != nil {
		t.Fatal(err)
	}
	fallback := provider.Provider{ID: "fallback", Chat: up.URL + "/v1", Key: "good-key", Models: []string{"bad"}, Fallback: []string{"relay/qwen"}}
	if err := provider.Save(fallback); err != nil {
		t.Fatal(err)
	}
	if err := provider.ConfigureModelHealth(provider.ModelHealthConfig{Enabled: true, IntervalMinutes: 30, FailureThreshold: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.ScanModelHealth(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := provider.SaveGroup(provider.Group{ID: "tested", Name: "Tested", Members: []string{"relay/bad", "relay/qwen"}, Routing: "order"}); err != nil {
		t.Fatal(err)
	}
	if err := provider.SaveGroup(provider.Group{ID: "blocked", Name: "Blocked", Members: []string{"relay/bad"}}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	clear(seen)
	mu.Unlock()
	for _, tc := range []struct {
		model string
		code  int
	}{
		{"relay/qwen", 200}, {"qwen", 200}, {"group/tested", 200}, {"fallback/bad", 200},
		{"relay/bad", 503}, {"group/blocked", 503}, {"relay/unlisted", 503},
	} {
		code, body := post(t, "/v1/chat/completions", `{"model":"`+tc.model+`","messages":[{"role":"user","content":"hi"}]}`)
		if code != tc.code {
			t.Errorf("%s: %d %s", tc.model, code, body)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || seen["Bearer good-key/qwen"] != 4 {
		t.Fatalf("gateway sent requests to unchecked keys/models: %+v", seen)
	}
}

// A provider can answer on Responses while its Chat endpoint is broken.
// Both passthrough and translation must use the checked endpoint.
func TestModelHealthGatewayUsesCheckedProtocol(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	var mu sync.Mutex
	seen := map[string]int{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" {
			w.Write([]byte(`{"data":[{"id":"qwen"}]}`))
			return
		}
		mu.Lock()
		seen[r.URL.Path]++
		mu.Unlock()
		if strings.Contains(r.URL.Path, "chat") {
			w.WriteHeader(503)
			w.Write([]byte(`{"error":{"message":"broken endpoint"}}`))
			return
		}
		var in struct{ Stream bool }
		json.NewDecoder(r.Body).Decode(&in)
		if in.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}]}}\n\n"))
		} else {
			w.Write([]byte(`{"id":"r","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`))
		}
	}))
	defer up.Close()
	p := provider.Provider{ID: "relay", Chat: up.URL + "/v1", Responses: up.URL + "/v1", Key: "k", Models: []string{"qwen"}}
	if err := provider.Save(p); err != nil {
		t.Fatal(err)
	}
	if err := provider.ConfigureModelHealth(provider.ModelHealthConfig{Enabled: true, IntervalMinutes: 30, FailureThreshold: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.ScanModelHealth(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	clear(seen)
	mu.Unlock()
	code, body := post(t, "/v1/chat/completions", `{"model":"relay/qwen","messages":[{"role":"user","content":"hi"}]}`)
	if code != 200 || !strings.Contains(body, "ok") {
		t.Fatalf("%d %s", code, body)
	}
	mu.Lock()
	defer mu.Unlock()
	if seen["/v1/chat/completions"] != 0 || seen["/v1/responses"] != 1 {
		t.Fatalf("wrong endpoint: %+v", seen)
	}
}

func TestModelHealthAttemptRechecksAfterPlanning(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	if err := provider.ConfigureModelHealth(provider.ModelHealthConfig{Enabled: true, IntervalMinutes: 30, FailureThreshold: 1}); err != nil {
		t.Fatal(err)
	}
	contacted := false
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { contacted = true; w.Write([]byte(`{}`)) }))
	defer up.Close()
	p := provider.Provider{ID: "relay", Chat: up.URL + "/v1", Key: "k"}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	code, _ := New().attempt(w, r, provider.Chat, p, "qwen", []byte(`{"model":"qwen","messages":[]}`), &Call{})
	if code != 503 || contacted {
		t.Fatalf("held attempt: status=%d contacted=%t", code, contacted)
	}
}

func TestModelHealthDeepSeekPrefixKeepsCheckedProtocol(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	var mu sync.Mutex
	responses := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" {
			w.Write([]byte(`{"data":[{"id":"deepseek-chat"}]}`))
			return
		}
		if strings.Contains(r.URL.Path, "responses") {
			mu.Lock()
			responses++
			mu.Unlock()
		}
		if strings.HasPrefix(r.URL.Path, "/beta/") {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":{"message":"not a chat model"}}`))
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer up.Close()
	p := provider.Provider{ID: "relay", Chat: up.URL + "/v1", Responses: up.URL + "/v1", Key: "k", Models: []string{"deepseek-chat"}}
	if err := provider.Save(p); err != nil {
		t.Fatal(err)
	}
	if err := provider.ConfigureModelHealth(provider.ModelHealthConfig{Enabled: true, IntervalMinutes: 30, FailureThreshold: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.ScanModelHealth(context.Background()); err != nil {
		t.Fatal(err)
	}
	res, proto, err := New().forwardTranslated(context.Background(), p, provider.Chat, &Request{Resume: true}, "deepseek-chat", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	mu.Lock()
	defer mu.Unlock()
	if res.StatusCode != http.StatusBadRequest || proto != provider.Chat || responses != 0 {
		t.Fatalf("prefix retry escaped checked endpoint: status=%d protocol=%s responses=%d", res.StatusCode, proto, responses)
	}
}
