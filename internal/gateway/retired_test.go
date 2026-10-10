package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

// zenDeprecated is OpenCode Zen's answer to a request for exo-free, as it
// was on 2026-10-10 (HTTP/2 410, content-type: application/json), while
// its /zen/v1/models still listed exo-free (MOMO on Discord).
const zenDeprecated = `{"type":"error","error":{"type":"ModelDeprecated","message":"Model exo-free has been deprecated."},"metadata":{"model":"exo-free"}}`

// zen answers exo-free as OpenCode Zen does, and every other model.
type zen struct{ tried []string }

func (z *zen) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	model := modelOf(body)
	z.tried = append(z.tried, model)
	w.Header().Set("Content-Type", "application/json")
	if model == "exo-free" {
		w.WriteHeader(http.StatusGone)
		io.WriteString(w, zenDeprecated)
		return
	}
	io.WriteString(w, `{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"from zen `+model+`"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
}

func listedModels(t *testing.T, s *Server) []string {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	var got struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("/v1/models: %v %s", err, rec.Body.String())
	}
	var ids []string
	for _, m := range got.Data {
		ids = append(ids, m.ID)
	}
	return ids
}

func TestRetiredModelLeavesTheLists(t *testing.T) {
	fresh(t)
	z := &zen{}
	serveOn(t, "opencode-zen-free", "k", []string{"exo-free", "space-bunny-free"}, z)
	s := New()
	if ids := listedModels(t, s); !slices.Contains(ids, "opencode-zen-free/exo-free") {
		t.Fatalf("before: %v", ids)
	}
	code, body := postAs(t, s, "", `{"model":"opencode-zen-free/exo-free","messages":[{"role":"user","content":"hi"}]}`)
	if code != http.StatusGone || !strings.Contains(body, "Model exo-free has been deprecated.") {
		t.Fatalf("the vendor's answer is the agent's: %d %s", code, body)
	}
	if !provider.Retired("opencode-zen-free", "exo-free") {
		t.Fatal("exo-free was not remembered as retired")
	}
	ids := listedModels(t, s)
	if slices.Contains(ids, "opencode-zen-free/exo-free") {
		t.Fatalf("a retired model still listed: %v", ids)
	}
	if !slices.Contains(ids, "opencode-zen-free/space-bunny-free") {
		t.Fatalf("the provider's other models went too: %v", ids)
	}
	// the provider's other models are served as before
	if code, body := postAs(t, s, "", `{"model":"opencode-zen-free/space-bunny-free","messages":[{"role":"user","content":"hi"}]}`); code != http.StatusOK {
		t.Fatalf("space-bunny-free: %d %s", code, body)
	}
	// a provider/model id is still sent: the vendor says what it says, and
	// an answer puts it back
	provider.Unretire("opencode-zen-free", "exo-free")
	if !slices.Contains(listedModels(t, s), "opencode-zen-free/exo-free") {
		t.Fatal("unretired, exo-free is still out of the list")
	}
}

func TestRetiredMemberFallsOver(t *testing.T) {
	fresh(t)
	z := &zen{}
	other := &keyed{}
	serveOn(t, "opencode-zen-free", "k", []string{"exo-free"}, z)
	serveOn(t, "b", "kb", []string{"exo-free"}, other)
	if err := provider.SaveGroup(provider.Group{
		Name: "Exo", Members: []string{"opencode-zen-free/exo-free", "b/exo-free"}, Routing: provider.Ordered,
	}); err != nil {
		t.Fatal(err)
	}
	s := New()
	code, body := postAs(t, s, "", `{"model":"group/exo","messages":[{"role":"user","content":"hi"}]}`)
	if code != http.StatusOK || !strings.Contains(body, "from kb") {
		t.Fatalf("the group's next member wasn't asked: %d %s", code, body)
	}
	if !provider.Retired("opencode-zen-free", "exo-free") || provider.Retired("b", "exo-free") {
		t.Fatal("retired on the wrong provider")
	}
	ids := listedModels(t, s)
	if slices.Contains(ids, "opencode-zen-free/exo-free") || !slices.Contains(ids, "b/exo-free") {
		t.Fatalf("lists: %v", ids)
	}
}

func TestRetiredWords(t *testing.T) {
	for _, c := range []struct {
		status int
		body   string
		want   bool
	}{
		{410, zenDeprecated, true},
		{404, `{"error":{"message":"The model text-davinci-003 has been deprecated","type":"invalid_request_error","code":"model_not_found"}}`, true},
		{400, `{"error":{"message":"max_tokens is deprecated, use max_completion_tokens"}}`, false},
		{410, `{"error":{"message":"gone"}}`, false},
		{429, zenDeprecated, false},
	} {
		if got := modelRetired(c.status, []byte(c.body)); got != c.want {
			t.Errorf("modelRetired(%d, %s) = %v", c.status, c.body, got)
		}
	}
}
