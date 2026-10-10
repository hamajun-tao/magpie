package provider

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/catalog"
)

// The models.dev catalog's anthropic rows, as models.dev served them on
// 2026-10-10: Opus 5.5, and Haiku 5.5, which it listed from 2026-10-07.
const (
	mdOpus55  = `"claude-opus-5-5":{"id":"claude-opus-5-5","name":"Claude Opus 5.5","family":"claude-opus","attachment":true,"reasoning":true,"reasoning_options":[{"type":"effort","values":["low","medium","high","xhigh","max"]}],"tool_call":true,"structured_output":true,"temperature":false,"knowledge":"2026-06","release_date":"2026-09-22","last_updated":"2026-09-22","modalities":{"input":["text","image","pdf"],"output":["text"]},"open_weights":false,"limit":{"context":1000000,"output":128000},"cost":{"input":4,"output":20,"cache_read":0.2,"cache_write":5}}`
	mdHaiku55 = `"claude-haiku-5-5":{"id":"claude-haiku-5-5","name":"Claude Haiku 5.5","family":"claude-haiku","attachment":true,"reasoning":true,"reasoning_options":[{"type":"toggle"},{"type":"effort","values":["low","medium","high","xhigh","max"]}],"tool_call":true,"structured_output":true,"temperature":false,"knowledge":"2026-06","release_date":"2026-10-07","last_updated":"2026-10-07","modalities":{"input":["text","image","pdf"],"output":["text"]},"open_weights":false,"limit":{"context":1000000,"output":128000},"cost":{"input":0.1,"output":0.5,"cache_read":0.01,"cache_write":0.125,"tiers":[{"input":0.5,"output":2.5,"cache_read":0.05,"cache_write":0.625,"tier":{"type":"context","size":100000}}]}}`
)

func writeAnthropicCatalog(t *testing.T, rows ...string) {
	t.Helper()
	body := `{"anthropic":{"id":"anthropic","name":"Anthropic","models":{`
	for i, r := range rows {
		if i > 0 {
			body += ","
		}
		body += r
	}
	body += `}}}`
	if err := os.MkdirAll(filepath.Dir(catalog.CachePath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(catalog.CachePath(), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	catalog.Reset()
}

// A Claude model models.dev lists after the Claude account's list was
// last fetched is served at once, as the catalog has it: the copy a fetch
// keeps doesn't hold it out until the next Refresh (wakaka on Discord:
// Haiku 5.5 in Claude Code on the same account, not in magpie).
func TestClaudeAccountServesModelsListedSinceItsFetch(t *testing.T) {
	home := claudeHome(t)
	claudeSignIn(t, home, time.Now().Add(time.Hour))
	t.Cleanup(catalog.Reset)

	writeAnthropicCatalog(t, mdOpus55)
	p, ok := find(All(), "claude")
	if !ok || p.Account == nil {
		t.Fatalf("claude: %+v %v", p, ok)
	}
	if _, err := p.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}

	// models.dev lists Haiku 5.5 now; the hourly sync wrote it
	writeAnthropicCatalog(t, mdOpus55, mdHaiku55)
	p, _ = find(All(), "claude")
	if _, ok := p.Fetched(); !ok {
		t.Fatal("the fetch left no list")
	}
	i := slices.IndexFunc(p.Available(), func(m catalog.Model) bool { return m.ID == "claude-haiku-5-5" })
	if i < 0 {
		t.Fatalf("Haiku 5.5 not served on the Claude account: %v", modelIDs(p.Available()))
	}
	h := p.Available()[i]
	if h.Name != "Claude Haiku 5.5" || h.Context != 1_000_000 || h.Output != 128_000 || !h.Reasoning ||
		!slices.Equal(h.Efforts, []string{"low", "medium", "high", "xhigh", "max"}) {
		t.Fatalf("Haiku 5.5: %+v", h)
	}
	if h.Price == nil || h.Price.Input != 0.1 || h.Price.Output != 0.5 {
		t.Fatalf("Haiku 5.5's price: %+v", h.Price)
	}
	var listed []string
	for _, e := range Catalog() {
		listed = append(listed, e.ID)
	}
	if !slices.Contains(listed, "claude/claude-haiku-5-5") {
		t.Fatalf("not among the models agents are offered: %v", listed)
	}
	if rp, model, ok := Resolve("claude/claude-haiku-5-5"); !ok || rp.ID != "claude" || model != "claude-haiku-5-5" {
		t.Fatalf("resolve: %+v %q %v", rp, model, ok)
	}

	// and a model models.dev no longer lists leaves the account's list too
	writeAnthropicCatalog(t, mdHaiku55)
	p, _ = find(All(), "claude")
	if got := modelIDs(p.Available()); !slices.Equal(got, []string{"claude-haiku-5-5"}) {
		t.Fatalf("after Opus 5.5 left the catalog: %v", got)
	}
}

// Factory's list is magpie's own too (droid's registry, compiled in): a
// model a newer magpie adds is served at once, not held out by the copy an
// older one's fetch kept.
func TestFactoryServesModelsAddedSinceItsFetch(t *testing.T) {
	claudeHome(t)
	p := factoryProvider(factoryLogin{Login: Login{User: "a@example.com"}})
	if _, err := p.Fetch(context.Background()); err != nil {
		t.Fatal(err)
	}
	old := factoryModels
	t.Cleanup(func() { factoryModels = old })
	factoryModels = append(slices.Clone(old), factoryModel{"claude-test-added", "Test Added", Anthropic, "anthropic", 200000, 64000, nil, true})
	if _, ok := p.Fetched(); !ok {
		t.Fatal("the fetch left no list")
	}
	if !slices.Contains(modelIDs(p.Available()), "claude-test-added") {
		t.Fatalf("a model added since the fetch: %v", modelIDs(p.Available()))
	}
}
