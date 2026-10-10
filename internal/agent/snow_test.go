package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/provider"
)

// snowDefault is a profile as Snow CLI 0.9.2 writes it for someone on a
// provider of their own, with settings magpie has nothing to do with.
const snowDefault = `{
  "snowcfg": {
    "baseUrl": "https://idealab.example.com/v1",
    "baseUrlMode": "auto",
    "apiKey": "team-key",
    "requestMethod": "chat",
    "advancedModel": "glm-5",
    "basicModel": "glm-5-air",
    "supportsVision": true,
    "maxContextTokens": 200000,
    "maxTokens": 64000,
    "streamingDisplay": false,
    "systemPromptId": "terse"
  },
  "companionMuted": true
}`

// snowWork is a second profile of the user's, signed in to a subscription
// through Snow's own OAuth.
const snowWork = `{
  "snowcfg": {
    "baseUrl": "https://api.example.com/v1",
    "apiKey": "",
    "requestMethod": "responses",
    "advancedModel": "kimi-k3",
    "basicModel": "kimi-k3",
    "oauth": {"provider": "example", "accessToken": "secret"}
  }
}`

func snowHome(t *testing.T) (home, dir string) {
	t.Helper()
	home = syncHome(t)
	if err := provider.Save(provider.Provider{ID: "deepseek", Name: "DeepSeek", Key: "k", Chat: "http://127.0.0.1:1/v1", Models: []string{"pro"}}); err != nil {
		t.Fatal(err)
	}
	dir = filepath.Join(home, ".snow")
	os.MkdirAll(filepath.Join(dir, "profiles"), 0o755)
	return home, dir
}

// snowUser lays out ~/.snow as Snow leaves it with the user's two
// profiles, default the active one.
func snowUser(t *testing.T, dir string) {
	t.Helper()
	os.WriteFile(filepath.Join(dir, "profiles", "default.json"), []byte(snowDefault), 0o644)
	os.WriteFile(filepath.Join(dir, "profiles", "work.json"), []byte(snowWork), 0o644)
	os.WriteFile(filepath.Join(dir, "active-profile.json"), []byte("{\n  \"activeProfile\": \"default\"\n}"), 0o644)
	os.WriteFile(filepath.Join(dir, "config.json"), []byte(snowDefault), 0o644)
}

func snowCfg(t *testing.T, path string) map[string]any {
	t.Helper()
	var f map[string]any
	if err := json.Unmarshal([]byte(readFile(path)), &f); err != nil {
		t.Fatalf("%s: %v\n%s", path, err, readFile(path))
	}
	sc, _ := f["snowcfg"].(map[string]any)
	if sc == nil {
		t.Fatalf("%s has no snowcfg:\n%s", path, readFile(path))
	}
	return sc
}

func snowActive(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, "active-profile.json"))
	if err != nil {
		return "(none)"
	}
	var v struct{ ActiveProfile string }
	json.Unmarshal(b, &v)
	return v.ActiveProfile
}

func TestSnow(t *testing.T) {
	_, dir := snowHome(t)
	snowUser(t, dir)
	a, err := Find("snow")
	if err != nil {
		t.Fatal(err)
	}
	if !a.Detected() || a.Dir != dir {
		t.Fatalf("not found in %s: %+v", dir, a)
	}
	if got := a.Values()["model"]; got != "glm-5" {
		t.Fatalf("model: %q", got)
	}
	// the picker: the user's profiles' models, then magpie's catalog
	opts := a.Field("model").Options(a.Values())
	if len(opts) < 3 || opts[0].Value != "glm-5" || opts[1].Value != "kimi-k3" || opts[0].Group != "Snow CLI" {
		t.Fatalf("own options: %+v", opts)
	}
	var via bool
	for _, o := range opts {
		via = via || o.Value == "magpie/deepseek/pro"
	}
	if !via {
		t.Fatalf("no magpie/deepseek/pro in %+v", opts)
	}

	if err := a.Apply("model", "magpie/deepseek/pro"); err != nil {
		t.Fatal(err)
	}
	ours := filepath.Join(dir, "profiles", "magpie.json")
	sc := snowCfg(t, ours)
	for k, want := range map[string]any{
		"baseUrl": gatewayV1(), "baseUrlMode": "base", "apiKey": gateway.TokenFor("snow"), "requestMethod": "chat",
		"advancedModel": "deepseek/pro", "basicModel": "deepseek/pro",
		// the user's own settings come along
		"streamingDisplay": false, "systemPromptId": "terse",
	} {
		if sc[k] != want {
			t.Errorf("snowcfg.%s = %v, want %v", k, sc[k], want)
		}
	}
	if !strings.Contains(readFile(ours), `"companionMuted": true`) {
		t.Errorf("the profile's other settings are gone:\n%s", readFile(ours))
	}
	if got := snowActive(dir); got != "magpie" {
		t.Fatalf("active profile %q", got)
	}
	// config.json is a copy of the active profile, as Snow's switch makes it
	if readFile(filepath.Join(dir, "config.json")) != readFile(ours) {
		t.Fatalf("config.json isn't magpie's profile:\n%s", readFile(filepath.Join(dir, "config.json")))
	}
	if readFile(filepath.Join(dir, "profiles", "default.json")) != snowDefault || readFile(filepath.Join(dir, "profiles", "work.json")) != snowWork {
		t.Fatal("a profile of the user's was rewritten")
	}
	if got := a.Values()["model"]; got != "magpie/deepseek/pro" {
		t.Fatalf("model after pick: %q", got)
	}
	if !a.Wired() {
		t.Fatal("not connected on magpie's model")
	}
	if msg := a.Check(); msg != "" {
		t.Fatalf("check: %s", msg)
	}
	// its own profile's list doesn't take magpie's for the user's
	for _, o := range a.Field("model").Options(a.Values()) {
		if o.Group == "Snow CLI" && o.Value == "deepseek/pro" {
			t.Fatalf("magpie's profile listed as the user's: %+v", o)
		}
	}

	// another of magpie's: the same profile, the model moved
	if err := a.Apply("model", "magpie/relay/glm-4.6"); err != nil {
		t.Fatal(err)
	}
	if sc := snowCfg(t, ours); sc["advancedModel"] != "relay/glm-4.6" || sc["streamingDisplay"] != false {
		t.Fatalf("second pick: %v", sc)
	}
	// glm-4.6's context, from the catalog
	if sc := snowCfg(t, ours); sc["maxContextTokens"] != float64(204800) {
		t.Fatalf("context: %v", sc["maxContextTokens"])
	}

	// back: the profile the user was on, magpie's gone
	if err := a.Apply("model", ""); err != nil {
		t.Fatal(err)
	}
	if got := snowActive(dir); got != "default" {
		t.Fatalf("active after reset: %q", got)
	}
	if _, err := os.Stat(ours); !os.IsNotExist(err) {
		t.Fatalf("magpie.json left: %v", err)
	}
	if readFile(filepath.Join(dir, "config.json")) != snowDefault {
		t.Fatalf("config.json not the user's profile again:\n%s", readFile(filepath.Join(dir, "config.json")))
	}
	if got := a.Values()["model"]; got != "glm-5" {
		t.Fatalf("model after reset: %q", got)
	}
	for k := range stashLoad() {
		if strings.HasPrefix(k, "snow:") {
			t.Errorf("stash keeps %s", k)
		}
	}
}

// A model of another profile of the user's, picked while on magpie's,
// switches Snow to that profile; its OAuth sign-in never comes into
// magpie's.
func TestSnowOwnProfile(t *testing.T) {
	_, dir := snowHome(t)
	snowUser(t, dir)
	os.WriteFile(filepath.Join(dir, "active-profile.json"), []byte(`{"activeProfile": "work"}`), 0o644)
	os.WriteFile(filepath.Join(dir, "config.json"), []byte(snowWork), 0o644)
	a, _ := Find("snow")
	if err := a.Apply("model", "magpie/deepseek/pro"); err != nil {
		t.Fatal(err)
	}
	ours := filepath.Join(dir, "profiles", "magpie.json")
	if body := readFile(ours); strings.Contains(body, "oauth") || strings.Contains(body, "secret") {
		t.Fatalf("the user's sign-in copied into magpie's profile:\n%s", body)
	}
	if sc := snowCfg(t, ours); sc["requestMethod"] != "chat" {
		t.Fatalf("requestMethod %v", sc["requestMethod"])
	}
	if err := a.Apply("model", "glm-5"); err != nil {
		t.Fatal(err)
	}
	if got := snowActive(dir); got != "default" {
		t.Fatalf("active %q, want default (the profile on glm-5)", got)
	}
	if _, err := os.Stat(ours); !os.IsNotExist(err) {
		t.Fatal("magpie.json left")
	}
	if readFile(filepath.Join(dir, "config.json")) != snowDefault {
		t.Fatal("config.json isn't default's")
	}
	if readFile(filepath.Join(dir, "profiles", "work.json")) != snowWork {
		t.Fatal("work.json rewritten")
	}
}

// A Snow from before profiles (config.json alone) and one never started
// (an empty ~/.snow) are each left as they were.
func TestSnowNoProfiles(t *testing.T) {
	t.Run("config.json alone", func(t *testing.T) {
		_, dir := snowHome(t)
		os.Remove(filepath.Join(dir, "profiles"))
		os.WriteFile(filepath.Join(dir, "config.json"), []byte(snowDefault), 0o644)
		a, _ := Find("snow")
		if got := a.Values()["model"]; got != "glm-5" {
			t.Fatalf("model: %q", got)
		}
		if err := a.Apply("model", "magpie/deepseek/pro"); err != nil {
			t.Fatal(err)
		}
		// Snow makes a default profile of config.json at its next start
		// when there is none: the user's, not magpie's
		if readFile(filepath.Join(dir, "profiles", "default.json")) != snowDefault {
			t.Fatalf("default.json:\n%s", readFile(filepath.Join(dir, "profiles", "default.json")))
		}
		if err := a.Apply("model", ""); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(dir, "active-profile.json")); !os.IsNotExist(err) {
			t.Fatal("active-profile.json left, there was none")
		}
		if readFile(filepath.Join(dir, "config.json")) != snowDefault {
			t.Fatal("config.json not the user's")
		}
		if got := a.Values()["model"]; got != "glm-5" {
			t.Fatalf("model after: %q", got)
		}
	})
	t.Run("never started", func(t *testing.T) {
		_, dir := snowHome(t)
		a, _ := Find("snow")
		if err := a.Apply("model", "magpie/deepseek/pro"); err != nil {
			t.Fatal(err)
		}
		sc := snowCfg(t, filepath.Join(dir, "profiles", "magpie.json"))
		if sc["baseUrl"] != gatewayV1() || sc["maxContextTokens"] != float64(snowContext) {
			t.Fatalf("%v", sc)
		}
		if err := a.Apply("model", ""); err != nil {
			t.Fatal(err)
		}
		for _, f := range []string{"active-profile.json", "config.json", "profiles/magpie.json", "profiles/default.json"} {
			if _, err := os.Stat(filepath.Join(dir, f)); !os.IsNotExist(err) {
				t.Errorf("%s left behind", f)
			}
		}
	})
}

// A profile of the user's own named magpie is theirs: not read as
// magpie's, and back as it was once magpie steps out.
func TestSnowUsersMagpieProfile(t *testing.T) {
	_, dir := snowHome(t)
	snowUser(t, dir)
	mine := filepath.Join(dir, "profiles", "magpie.json")
	os.WriteFile(mine, []byte(snowWork), 0o644)
	os.WriteFile(filepath.Join(dir, "active-profile.json"), []byte(`{"activeProfile": "magpie"}`), 0o644)
	a, _ := Find("snow")
	if got := a.Values()["model"]; got != "kimi-k3" || a.Wired() {
		t.Fatalf("the user's own magpie profile reads %q, wired %v", got, a.Wired())
	}
	if err := a.Apply("model", "magpie/deepseek/pro"); err != nil {
		t.Fatal(err)
	}
	if sc := snowCfg(t, mine); sc["apiKey"] != gateway.TokenFor("snow") {
		t.Fatalf("%v", sc)
	}
	if err := a.Apply("model", ""); err != nil {
		t.Fatal(err)
	}
	if readFile(mine) != snowWork || snowActive(dir) != "magpie" {
		t.Fatalf("the user's magpie profile not back (active %q):\n%s", snowActive(dir), readFile(mine))
	}
}

// Something else rewrote magpie's profile: Check says what, and Sync puts
// the gateway back; $SNOW_CONFIG_DIR is where Snow looks.
func TestSnowCheckSync(t *testing.T) {
	home, _ := snowHome(t)
	dir := filepath.Join(home, "elsewhere")
	t.Setenv("SNOW_CONFIG_DIR", dir)
	os.MkdirAll(filepath.Join(dir, "profiles"), 0o755)
	snowUser(t, dir)
	a, _ := Find("snow")
	if a.Dir != dir {
		t.Fatalf("dir %s", a.Dir)
	}
	if err := a.Apply("model", "magpie/deepseek/pro"); err != nil {
		t.Fatal(err)
	}
	ours := filepath.Join(dir, "profiles", "magpie.json")
	os.WriteFile(ours, []byte(strings.Replace(readFile(ours), gatewayV1(), "http://127.0.0.1:9/v1", 1)), 0o644)
	if msg := a.Check(); !strings.Contains(msg, "baseUrl") {
		t.Fatalf("check: %q", msg)
	}
	if err := a.Sync(); err != nil {
		t.Fatal(err)
	}
	if msg := a.Check(); msg != "" {
		t.Fatalf("after sync: %s", msg)
	}
	if readFile(filepath.Join(dir, "config.json")) != readFile(ours) {
		t.Fatal("config.json not synced")
	}
	if _, err := os.Stat(filepath.Join(home, ".snow", "profiles", "magpie.json")); !os.IsNotExist(err) {
		t.Fatal("wrote ~/.snow with SNOW_CONFIG_DIR set")
	}
}
