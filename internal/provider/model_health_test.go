package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/yetone/magpie/internal/catalog"
)

func healthHome(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
}

func enableHealth(t *testing.T, failures int) {
	t.Helper()
	if err := ConfigureModelHealth(ModelHealthConfig{Enabled: true, IntervalMinutes: 30, FailureThreshold: failures}); err != nil {
		t.Fatal(err)
	}
}

// A relay's list is not proof: a listed model can fail on one key while
// another works. Checks preserve picks, hide failures, and later recover.
func TestModelHealthScanKeysAndRecovery(t *testing.T) {
	healthHome(t)
	var mu sync.Mutex
	broken := false
	seen := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.Write([]byte(`{"data":[{"id":"qwen-test"},{"id":"gone"},{"id":"empty"}]}`))
			return
		}
		var in struct {
			Model    string
			Messages []struct{ Content string }
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Error(err)
		}
		if len(in.Messages) != 1 || in.Messages[0].Content != "不要问为什么，只回复ok" {
			t.Errorf("wrong test prompt: %+v", in)
		}
		mu.Lock()
		seen[r.Header.Get("Authorization")+"/"+in.Model]++
		fail := broken || in.Model == "gone" || r.Header.Get("Authorization") == "Bearer broken-key"
		mu.Unlock()
		if fail {
			w.WriteHeader(404)
			w.Write([]byte(`{"error":{"message":"model not available"}}`))
		} else if in.Model == "empty" {
			w.Write([]byte(`{}`))
		} else {
			w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
		}
	}))
	defer srv.Close()
	p := Provider{ID: "relay", Chat: srv.URL + "/v1", Key: "broken-key", Keys: []KeyAccount{{Key: "working-key"}}, Models: []string{"qwen-test", "gone", "empty", "hand-written"}}
	if err := Save(p); err != nil {
		t.Fatal(err)
	}
	enableHealth(t, 2)
	if len(p.Exposed()) != 0 {
		t.Fatal("unverified models exposed")
	}
	scan := func() ModelHealthState {
		t.Helper()
		s, err := ScanModelHealth(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	s := scan()
	if s.LastScan.IsZero() || len(s.Records) != 8 {
		t.Fatalf("state: %+v", s)
	}
	keys := p.KeysOn()
	if ModelHealthAllows(p.WithKey(keys[0]), "qwen-test") || !ModelHealthAllows(p.WithKey(keys[1]), "qwen-test") {
		t.Fatal("the two keys did not get independent results")
	}
	ms := p.Exposed()
	if len(ms) != 2 || ms[0].ID != "qwen-test" || ms[1].ID != "hand-written" {
		t.Fatalf("exposed: %+v", ms)
	}
	ps, err := Stored()
	if err != nil || len(ps) != 1 || !slices.Equal(ps[0].Models, p.Models) {
		t.Fatalf("picks changed: %+v, %v", ps, err)
	}
	mu.Lock()
	broken = true
	mu.Unlock()
	scan()
	if !ModelHealthAllows(p.WithKey(keys[1]), "qwen-test") {
		t.Fatal("one transient failure hid a verified model")
	}
	scan()
	if ModelHealthAllows(p.WithKey(keys[1]), "qwen-test") || len(p.Exposed()) != 0 {
		t.Fatal("repeated failures were not quarantined")
	}
	mu.Lock()
	broken = false
	mu.Unlock()
	scan()
	if !ModelHealthAllows(p.WithKey(keys[1]), "qwen-test") {
		t.Fatal("quarantined model was not retried and restored")
	}
	mu.Lock()
	defer mu.Unlock()
	if seen["Bearer broken-key/qwen-test"] != 4 || seen["Bearer working-key/qwen-test"] != 4 {
		t.Fatalf("seen: %+v", seen)
	}
}

func TestModelHealthIdentityAndDisabled(t *testing.T) {
	healthHome(t)
	p := Provider{ID: "one", Chat: "https://example.invalid/v1", Key: "secret", Models: []string{"qwen"}}
	if !ModelHealthAllows(p, "qwen") {
		t.Fatal("default checks must be off")
	}
	enableHealth(t, 1)
	if err := saveHealthResult(context.Background(), p, "qwen", Result{OK: true}); err != nil {
		t.Fatal(err)
	}
	if !ModelHealthAllows(p, "qwen") {
		t.Fatal("successful check not allowed")
	}
	for _, change := range []func(*Provider){
		func(q *Provider) { q.ID = "two" },
		func(q *Provider) { q.Key = "changed" },
		func(q *Provider) { q.Chat = "https://different.invalid/v1" },
		func(q *Provider) { q.Headers = map[string]string{"X-Route": "changed"} },
	} {
		q := p
		change(&q)
		if ModelHealthAllows(q, "qwen") {
			t.Fatalf("changed provider trusted old result: %+v", q)
		}
	}
	if ModelHealthAllows(p, "new-model") {
		t.Fatal("new model trusted old result")
	}
	b, err := os.ReadFile(ModelHealthPath())
	if err != nil || strings.Contains(string(b), "secret") {
		t.Fatal("health state leaked the key")
	}
	s, err := LoadModelHealth()
	if err != nil {
		t.Fatal(err)
	}
	clear(s.Records)
	if !ModelHealthAllows(p, "qwen") {
		t.Fatal("caller mutated shared health records")
	}
	s.Enabled = false
	if err := ConfigureModelHealth(s.ModelHealthConfig); err != nil {
		t.Fatal(err)
	}
	if !ModelHealthAllows(p, "new-model") {
		t.Fatal("turning off did not restore normal behavior")
	}
}

func TestModelHealthDoesNotModifyCatalog(t *testing.T) {
	healthHome(t)
	enableHealth(t, 1)
	p := Provider{ID: "relay", Chat: "https://example.invalid/v1"}
	ms := []catalog.Model{{ID: "hidden"}, {ID: "ready"}}
	if err := saveHealthResult(context.Background(), p, "ready", Result{OK: true}); err != nil {
		t.Fatal(err)
	}
	got := healthExposed(p, ms)
	if len(got) != 1 || got[0].ID != "ready" || ms[0].ID != "hidden" || ms[1].ID != "ready" {
		t.Fatalf("filtered=%+v source=%+v", got, ms)
	}
}

func TestModelHealthLargeListKeepsVerifiedModels(t *testing.T) {
	healthHome(t)
	enableHealth(t, 1)
	p := Provider{ID: "relay", Chat: "https://example.invalid/v1"}
	var ms []catalog.Model
	for i := range manyModels + 1 {
		ms = append(ms, catalog.Model{ID: fmt.Sprintf("qwen-%d", i)})
	}
	if err := catalog.SaveLive(p.ID, p.Chat, ms); err != nil {
		t.Fatal(err)
	}
	if err := saveHealthResult(context.Background(), p, ms[manyModels].ID, Result{OK: true}); err != nil {
		t.Fatal(err)
	}
	got := p.Exposed()
	if len(got) != 1 || got[0].ID != ms[manyModels].ID {
		t.Fatalf("verified model lost behind picker limit: %+v", got)
	}
}

func TestModelHealthRepairsURLAndRedactsErrors(t *testing.T) {
	healthHome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Write([]byte(`{"data":[{"id":"qwen"}]}`))
			return
		}
		if r.URL.Path != "/v1/chat/completions" {
			w.WriteHeader(404)
			return
		}
		w.WriteHeader(401)
		fmt.Fprintf(w, `{"error":{"message":"%s %s"}}`, r.Header.Get("Authorization"), r.Header.Get("X-Api-Key"))
	}))
	defer srv.Close()
	p := Provider{ID: "relay", Chat: srv.URL, Key: "secret-primary", Headers: map[string]string{"X-Api-Key": "secret-custom"}}
	if err := Save(p); err != nil {
		t.Fatal(err)
	}
	enableHealth(t, 1)
	s, err := ScanModelHealth(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := Find(p.ID)
	if err != nil || fresh.Chat != srv.URL+"/v1" {
		t.Fatalf("URL repair: %+v %v", fresh, err)
	}
	if _, ok := s.Records[healthRecordID(*fresh, "qwen")]; !ok {
		t.Fatal("scan recorded the old destination")
	}
	b, err := os.ReadFile(ModelHealthPath())
	if err != nil || strings.Contains(string(b), "secret-primary") || strings.Contains(string(b), "secret-custom") {
		t.Fatalf("health state leaked credentials: %s %v", b, err)
	}
}

func TestModelHealthAcceptsUnrequestedStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n"))
	}))
	defer srv.Close()
	p := Provider{ID: "relay", Chat: srv.URL}
	if r := p.healthTest(context.Background(), "qwen"); !r.OK {
		t.Fatalf("valid SSE response: %+v", r)
	}
}

func TestModelHealthCorruptionAndScanLock(t *testing.T) {
	healthHome(t)
	enableHealth(t, 1)
	unlock, err := lockModelHealth(context.Background(), ".scan.lock", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ScanModelHealth(context.Background()); !errors.Is(err, ErrModelHealthScanning) {
		t.Errorf("overlapping scan: %v", err)
	}
	unlock()
	if err := os.WriteFile(ModelHealthPath(), []byte("broken-json"), 0600); err != nil {
		t.Fatal(err)
	}
	p := Provider{ID: "relay", Chat: "https://example.invalid/v1"}
	if ModelHealthAllows(p, "qwen") {
		t.Fatal("unreadable state was treated as disabled")
	}
	if _, err := ScanModelHealth(context.Background()); err == nil {
		t.Fatal("scan overwrote corrupt state")
	}
	if err := ConfigureModelHealth(ModelHealthConfig{IntervalMinutes: 30, FailureThreshold: 2}); err == nil {
		t.Fatal("configure overwrote corrupt state")
	}
	b, _ := os.ReadFile(ModelHealthPath())
	if string(b) != "broken-json" {
		t.Fatalf("corrupt state changed: %s", b)
	}
}

func TestModelHealthCancellationDoesNotQuarantine(t *testing.T) {
	healthHome(t)
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.Write([]byte(`{"data":[{"id":"qwen"}]}`))
			return
		}
		io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
	}))
	defer srv.Close()
	p := Provider{ID: "relay", Chat: srv.URL + "/v1", Key: "k", Models: []string{"qwen"}}
	if err := Save(p); err != nil {
		t.Fatal(err)
	}
	enableHealth(t, 1)
	if err := saveHealthResult(context.Background(), p, "qwen", Result{OK: true}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := ScanModelHealth(ctx); done <- err }()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("scan: %v", err)
	}
	if !ModelHealthAllows(p, "qwen") {
		t.Fatal("cancelled scan quarantined the model")
	}
}

func TestModelHealthAnswerValidation(t *testing.T) {
	for _, tc := range []struct {
		body string
		ok   bool
	}{
		{`{}`, false}, {`<html>sign in</html>`, false},
		{`{"error":{"message":"quota"},"choices":[{"message":{"content":"ok"}}]}`, false},
		{`{"choices":[{"message":{"content":" "}}]}`, false},
		{`{"choices":[{"message":{"content":"ok"}}]}`, true},
		{`{"choices":[{"message":{"reasoning_content":"thinking"}}]}`, true},
		{`{"content":[{"type":"text","text":"ok"}]}`, true},
		{`{"output":[{"content":[{"type":"output_text","text":"ok"}]}]}`, true},
	} {
		if got := healthAnswer([]byte(tc.body)); got != tc.ok {
			t.Errorf("%s: %t", tc.body, got)
		}
	}
	for _, tc := range []struct {
		body string
		ok   bool
	}{
		{"data: [DONE]\n\n", false},
		{"data: {\"choices\":[{\"delta\":{\"content\":\" \"}}]}\n\n", false},
		{"data: {\"choices\":[{\"delta\":{\"tool_calls\":[]}}]}\n\n", false},
		{"data: {\"type\":\"response.output_item.added\",\"item\":{}}\n\n", false},
		{"data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n", true},
		{"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n", true},
		{"data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n", true},
	} {
		problem, failed := streamAnswerMode(strings.NewReader(tc.body), true)
		got := !failed
		if got != tc.ok {
			t.Errorf("stream %s: %t (%s)", tc.body, got, problem)
		}
	}
}

// Retrieval and Gemini-only models cannot answer a Chat/Responses/Messages
// probe. They must remain exposed, and no conversation probe may be sent.
func TestModelHealthSkipsNonConversationEndpoints(t *testing.T) {
	healthHome(t)
	enableHealth(t, 1)
	var mu sync.Mutex
	seen := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.WriteHeader(503) // keep the cached per-model protocol metadata
			return
		}
		var in struct{ Model string }
		json.NewDecoder(r.Body).Decode(&in)
		mu.Lock()
		seen[in.Model]++
		mu.Unlock()
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()
	p := Provider{ID: "relay", Chat: srv.URL + "/v1", Gemini: srv.URL + "/v1beta", Models: []string{"qwen", "text-embedding-3-small", "bge-reranker-v2-m3", "gemini-native"}}
	if err := Save(p); err != nil {
		t.Fatal(err)
	}
	ms := []catalog.Model{{ID: "qwen"}, {ID: "text-embedding-3-small"}, {ID: "bge-reranker-v2-m3"}, {ID: "gemini-native", APIs: []string{"gemini"}}}
	if err := catalog.SaveLive(p.ID, p.Chat, ms); err != nil {
		t.Fatal(err)
	}
	s, err := ScanModelHealth(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Records) != 1 {
		t.Errorf("non-conversation models probed: %+v", s.Records)
	}
	for _, id := range p.Models {
		if !ModelHealthAllows(p, id) {
			t.Errorf("model %s wrongly blocked", id)
		}
	}
	if got := p.Exposed(); len(got) != len(ms) {
		t.Errorf("models wrongly hidden: %+v", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || seen["qwen"] != 1 {
		t.Fatalf("wrong probes sent: %+v", seen)
	}
}
