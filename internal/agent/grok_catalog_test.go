package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/provider"
)

// These expectations use the IDs Grok receives from /v1/models. Registering
// an extra magpie/ alias splits the model's identity from its capabilities.
func TestGrokCanonicalModelCarriesItsCapabilities(t *testing.T) {
	home, path := grokSandbox(t)
	effortModels(t)
	a := grok(home)
	if err := a.Field("model").Set("magpie/volc/lo-hi-max"); err != nil {
		t.Fatal(err)
	}
	doc := grokCatalogConfig(t, path)
	models := doc["model"].(map[string]any)
	model, ok := models["volc/lo-hi-max"].(map[string]any)
	if !ok {
		t.Fatalf("the public catalog ID has no registration:\n%s", readFile(path))
	}
	if model["model"] != "volc/lo-hi-max" || model["model_provider"] != "magpie" {
		t.Fatalf("canonical registration points elsewhere: %v", model)
	}
	if got := model["reasoning_efforts"]; !slices.Equal(grokStringArray(got), []string{"low", "high", "max"}) {
		t.Fatalf("canonical model's reasoning levels: %v", got)
	}
	if _, ok := models["magpie/volc/lo-hi-max"]; ok {
		t.Fatal("a second registration kept the old alias")
	}
	if got := doc["models"].(map[string]any)["default"]; got != "volc/lo-hi-max" {
		t.Fatalf("Grok's default = %v, want the public catalog ID", got)
	}
	if got := a.Field("model").Get(); got != "magpie/volc/lo-hi-max" {
		t.Fatalf("Magpie lost connection tracking: %q", got)
	}
	if got := a.Check(); got != "" {
		t.Fatalf("canonical registration isn't recognized as connected: %s", got)
	}
	for _, o := range a.Field("model").Options(a.Values()) {
		if o.Value == "volc/lo-hi-max" {
			t.Fatalf("a managed canonical table was offered as a direct model: %+v", o)
		}
	}
}

func TestGrokLegacyCatalogMigratesDefaultWindowAndCache(t *testing.T) {
	home, path := grokSandbox(t)
	const old = `# keep my UI
[models]
default = "magpie/deepseek/pro"
default_reasoning_effort = "high"

[model."magpie/deepseek/pro"]
model = "deepseek/pro"
base_url = "http://127.0.0.1:3425/v1"
api_key = "magpie"
api_backend = "chat_completions"
context_window = 300000

[model."magpie/deepseek/flash"]
model = "deepseek/flash"
base_url = "http://127.0.0.1:3425/v1"
api_key = "magpie"
api_backend = "chat_completions"
context_window = 200000

[model.native]
model = "grok-native"
base_url = "https://native.example/v1"
api_key = "mine"
`
	grokCatalogWrite(t, path, old)
	cache := filepath.Join(filepath.Dir(path), "models_cache.json")
	const cached = `{"fetched_at":"2026-11-03T21:00:00Z","grok_version":"1.0.70","models":{
 "magpie/deepseek/pro":{"info":{"id":"magpie/deepseek/pro","model":"deepseek/pro"}},
 "magpie/deepseek/flash":{"info":{"id":"magpie/deepseek/flash","model":"deepseek/flash"}},
 "deepseek/pro":{"info":{"id":"deepseek/pro","name":"Pro"}},
 "grok-native":{"info":{"id":"grok-native","name":"Native"}}}}`
	grokCatalogWrite(t, cache, cached)
	a := grok(home)
	if err := a.Sync(); err != nil {
		t.Fatal(err)
	}
	doc := grokCatalogConfig(t, path)
	if got := doc["models"].(map[string]any)["default"]; got != "deepseek/pro" {
		t.Fatalf("legacy default wasn't migrated: %v", got)
	}
	if pro, flash := grokWindows(t, path); pro != 300000 || flash != 200000 {
		t.Fatalf("migration changed context windows: pro %d, flash %d", pro, flash)
	}
	if strings.Contains(readFile(path), `[model."magpie/deepseek/`) {
		t.Fatalf("old alias registrations survived:\n%s", readFile(path))
	}
	if !strings.Contains(readFile(path), "# keep my UI") || !strings.Contains(readFile(path), `[model.native]`) {
		t.Fatalf("migration touched user configuration:\n%s", readFile(path))
	}
	var decoded struct {
		Fetched string                     `json:"fetched_at"`
		Models  map[string]json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal([]byte(readFile(cache)), &decoded); err != nil {
		t.Fatal(err)
	}
	if _, ok := decoded.Models["magpie/deepseek/pro"]; ok {
		t.Fatalf("cached alias survived migration: %s", readFile(cache))
	}
	if len(decoded.Models) != 2 || decoded.Models["grok-native"] == nil || decoded.Models["deepseek/pro"] == nil ||
		decoded.Fetched != "2026-11-03T21:00:00Z" {
		t.Fatalf("unrelated cached model data changed: %s", readFile(cache))
	}
	configOnce, cacheOnce := readFile(path), readFile(cache)
	if err := a.Sync(); err != nil {
		t.Fatal(err)
	}
	if readFile(path) != configOnce || readFile(cache) != cacheOnce {
		t.Fatal("a second sync wasn't idempotent")
	}
}

func TestGrokCatalogReplacesOwnedReasoningSubtables(t *testing.T) {
	for _, tc := range []struct {
		name, model, child, ownership string
	}{
		{"legacy", `"magpie/volc/lo-hi-max"`, `"magpie/volc/lo-hi-max"`, ""},
		{"legacy-literal-child", `"magpie/volc/lo-hi-max"`, `'magpie/volc/lo-hi-max'`, ""},
		{"canonical", `"volc/lo-hi-max"`, `"volc/lo-hi-max"`, "model_provider = \"magpie\"\n"},
		{"canonical-literal-child", `"volc/lo-hi-max"`, `'volc/lo-hi-max'`, "model_provider = \"magpie\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, path := grokSandbox(t)
			effortModels(t)
			input := fmt.Sprintf(`[models]
default = %s
default_reasoning_effort = "max"

[model_providers.magpie]

[model.%s]
model = "volc/lo-hi-max"
%sbase_url = "http://127.0.0.1:3425/v1"
api_key = "magpie"
context_window = 45000

[[model.%s.reasoning_efforts]]
value = "high"
label = "High"

[[model.%s.reasoning_efforts]]
value = "max"
label = "Max"
default = true

[model."magpie/user-model"]
model = "user-model"
api_key = "private"

[[model."magpie/user-model".reasoning_efforts]]
value = "high"
label = "My level"
`, tc.model, tc.model, tc.ownership, tc.child, tc.child)
			grokCatalogWrite(t, path, input)
			if err := grok(home).Sync(); err != nil {
				t.Fatal(err)
			}
			doc := grokCatalogConfig(t, path)
			models := doc["model"].(map[string]any)
			if _, exists := models["magpie/volc/lo-hi-max"]; exists {
				t.Fatalf("reasoning child tables recreated the removed alias:\n%s", readFile(path))
			}
			m := models["volc/lo-hi-max"].(map[string]any)
			if !slices.Equal(grokStringArray(m["reasoning_efforts"]), []string{"low", "high", "max"}) ||
				m["context_window"] != int64(45000) {
				t.Fatalf("canonical model lost capabilities or the user window: %v", m)
			}
			if doc["models"].(map[string]any)["default_reasoning_effort"] != "max" {
				t.Fatal("migration changed the user's effort default")
			}
			if !strings.Contains(readFile(path), "[[model.\"magpie/user-model\".reasoning_efforts]]\nvalue = \"high\"\nlabel = \"My level\"") {
				t.Fatalf("an unowned model's reasoning table changed:\n%s", readFile(path))
			}
		})
	}
}

func TestGrokLegacyCatalogMovesUserSubtablesWithoutAliases(t *testing.T) {
	home, path := grokSandbox(t)
	const input = `[models]
default = "magpie/deepseek/pro"

[model."magpie/deepseek/pro"]
model = "deepseek/pro"
base_url = "http://127.0.0.1:3425/v1"
api_key = "magpie"
context_window = 45000

[model.'magpie/deepseek/pro'.extra_headers]
X-Proxy-Route = "keep"
`
	grokCatalogWrite(t, path, input)
	a := grok(home)
	if err := a.Sync(); err != nil {
		t.Fatal(err)
	}
	doc := grokCatalogConfig(t, path)
	models := doc["model"].(map[string]any)
	if _, exists := models["magpie/deepseek/pro"]; exists {
		t.Fatalf("a user subtable recreated the removed alias:\n%s", readFile(path))
	}
	m := models["deepseek/pro"].(map[string]any)
	headers, ok := m["extra_headers"].(map[string]any)
	if !ok || headers["X-Proxy-Route"] != "keep" {
		t.Fatalf("migration lost a model's proxy headers: %v", m)
	}
	if err := a.Field("model").Set(""); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(readFile(path), "deepseek/pro") || strings.Contains(readFile(path), "extra_headers") {
		t.Fatalf("disconnect left a child table to recreate the model:\n%s", readFile(path))
	}
}

func TestGrokLegacyCatalogKeepsRecordedProxyRoute(t *testing.T) {
	home, path := grokSandbox(t)
	a := grok(home)
	if err := a.Field("model").Set("magpie/deepseek/pro"); err != nil {
		t.Fatal(err)
	}
	// A saved connection record proves these old tables belong to Magpie
	// even after the user points them at a remote Magpie and its own key.
	const input = `[models]
default = "magpie/deepseek/pro"

[model."magpie/deepseek/pro"]
model = "deepseek/pro"
base_url = "https://proxy.example/magpie/v1"
api_key = "remote-key"
api_backend = "chat_completions"
context_window = 300000

[model."magpie/deepseek/flash"]
model = "deepseek/flash"
base_url = "https://proxy.example/magpie/v1"
api_key = "remote-key"
api_backend = "chat_completions"
context_window = 200000
`
	grokCatalogWrite(t, path, input)
	if err := synced(a.Sync); err != nil {
		t.Fatal(err)
	}
	doc := grokCatalogConfig(t, path)
	for _, id := range []string{"deepseek/pro", "deepseek/flash"} {
		m := doc["model"].(map[string]any)[id].(map[string]any)
		if m["base_url"] != "https://proxy.example/magpie/v1" || m["api_key"] != "remote-key" ||
			m["api_backend"] != "chat_completions" {
			t.Fatalf("%s lost its route during migration: %v", id, m)
		}
	}
}

func TestGrokUnreadCacheStopsMigration(t *testing.T) {
	for _, cached := range []string{`{"models":`, `{"models":"not an object"}`} {
		t.Run(cached, func(t *testing.T) {
			home, path := grokSandbox(t)
			const input = `[models]
default = "magpie/deepseek/pro"

[model."magpie/deepseek/pro"]
model = "deepseek/pro"
base_url = "http://127.0.0.1:3425/v1"
api_key = "magpie"
`
			grokCatalogWrite(t, path, input)
			cache := filepath.Join(filepath.Dir(path), "models_cache.json")
			grokCatalogWrite(t, cache, cached)
			if err := grok(home).Sync(); err == nil || !strings.Contains(err.Error(), cache) {
				t.Fatalf("an unknown cache was treated as empty: %v", err)
			}
			if readFile(path) != input || readFile(cache) != cached {
				t.Fatalf("migration changed files after a cache read failed:\n%s\n%s", readFile(path), readFile(cache))
			}
		})
	}
}

func TestGrokCanonicalCollisionLeavesUserModelAndLegacyConfig(t *testing.T) {
	for _, action := range []string{"connect", "sync"} {
		t.Run(action, func(t *testing.T) {
			home, path := grokSandbox(t)
			const input = `[models]
default = "magpie/deepseek/pro"

[model."magpie/deepseek/pro"]
model = "deepseek/pro"
base_url = "http://127.0.0.1:3425/v1"
api_key = "magpie"

[model."deepseek/pro"]
model = "my-private-model"
base_url = "https://my-relay.example/v1"
api_key = "my-private-key"
context_window = 45000
`
			grokCatalogWrite(t, path, input)
			a := grok(home)
			var err error
			if action == "connect" {
				err = a.Field("model").Set("magpie/deepseek/pro")
			} else {
				err = a.Sync()
			}
			if err == nil || !strings.Contains(err.Error(), "deepseek/pro") {
				t.Fatalf("canonical user-table collision wasn't reported: %v", err)
			}
			if got := readFile(path); got != input {
				t.Fatalf("collision changed user config:\n%s", got)
			}
		})
	}
}

func TestGrokPrefixAloneDoesNotOwnAUserModel(t *testing.T) {
	home, path := grokSandbox(t)
	const user = `[model."magpie/my-own"]
model = "my-own"
base_url = "https://my-relay.example/v1"
api_key = "my-key"
context_window = 45000
`
	grokCatalogWrite(t, path, "[models]\ndefault = \"grok-native\"\n\n"+user)
	a := grok(home)
	if err := a.Sync(); err != nil {
		t.Fatal(err)
	}
	if got := readFile(path); got != "[models]\ndefault = \"grok-native\"\n\n"+user {
		t.Fatalf("an unowned prefix table triggered catalog sync:\n%s", got)
	}
	if err := a.Pick("model", "magpie/deepseek/pro"); err != nil {
		t.Fatal(err)
	}
	if err := a.Pick("model", "grok-native"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readFile(path), user) {
		t.Fatalf("a user table sharing the old prefix was removed:\n%s", readFile(path))
	}
}

func TestGrokCanonicalDisconnectRestoresNativeModel(t *testing.T) {
	home, path := grokSandbox(t)
	const native = `[model.native]
model = "grok-native"
base_url = "https://native.example/v1"
api_key = "mine"
`
	grokCatalogWrite(t, path, "# mine\n[models]\ndefault = \"grok-native\"\n\n"+native)
	a := grok(home)
	if err := a.Pick("model", "magpie/deepseek/pro"); err != nil {
		t.Fatal(err)
	}
	if !a.Wired() {
		t.Fatalf("a canonical default isn't connected:\n%s", readFile(path))
	}
	if err := a.Disconnect(); err != nil {
		t.Fatal(err)
	}
	doc := grokCatalogConfig(t, path)
	if doc["models"].(map[string]any)["default"] != "grok-native" || strings.Contains(readFile(path), "model_provider") ||
		!strings.Contains(readFile(path), native) || strings.Contains(readFile(path), "deepseek/") {
		t.Fatalf("disconnect didn't restore the native route:\n%s", readFile(path))
	}
	if err := a.Sync(); err != nil {
		t.Fatal(err)
	}
	if a.Wired() || a.Field("model").Get() != "grok-native" {
		t.Fatalf("sync connected the agent after disconnect:\n%s", readFile(path))
	}
	if err := a.Pick("model", "magpie/deepseek/flash"); err != nil {
		t.Fatal(err)
	}
	if got := grokCatalogConfig(t, path)["models"].(map[string]any)["default"]; got != "deepseek/flash" {
		t.Fatalf("reconnect restored an old alias: %v", got)
	}
}

func TestGrokCanonicalCheckNamesMissingRegistration(t *testing.T) {
	home, path := grokSandbox(t)
	a := grok(home)
	if err := a.Field("model").Set("magpie/deepseek/pro"); err != nil {
		t.Fatal(err)
	}
	if err := edit.DelTOMLTable(path, `model."deepseek/pro"`); err != nil {
		t.Fatal(err)
	}
	if got := a.Check(); !strings.Contains(got, `[model."deepseek/pro"]`) || !strings.Contains(got, "gone") {
		t.Fatalf("a missing canonical registration was not diagnosed: %q", got)
	}
}

func TestGrokCanonicalCheckNamesMissingProvider(t *testing.T) {
	home, path := grokSandbox(t)
	a := grok(home)
	if err := a.Field("model").Set("magpie/deepseek/pro"); err != nil {
		t.Fatal(err)
	}
	if err := edit.DelTOMLTable(path, "model_providers.magpie"); err != nil {
		t.Fatal(err)
	}
	if got := a.Check(); !strings.Contains(got, "[model_providers.magpie]") || !strings.Contains(got, "gone") {
		t.Fatalf("a missing native provider relationship was not diagnosed: %q", got)
	}
}

func TestGrokCanonicalCheckAcceptsLiteralQuotedTables(t *testing.T) {
	home, path := grokSandbox(t)
	a := grok(home)
	if err := a.Field("model").Set("magpie/deepseek/pro"); err != nil {
		t.Fatal(err)
	}
	input := strings.ReplaceAll(readFile(path), `model."deepseek/pro"`, `model.'deepseek/pro'`)
	grokCatalogWrite(t, path, input)
	if got := a.Check(); got != "" {
		t.Fatalf("a different valid header spelling was mistaken for drift: %q", got)
	}
	if err := a.Sync(); err != nil {
		t.Fatal(err)
	}
	if got := grokCatalogConfig(t, path)["models"].(map[string]any)["default"]; got != "deepseek/pro" {
		t.Fatalf("sync changed the canonical default: %v", got)
	}
}

func TestGrokDisconnectKeepsAnExistingProviderDefinition(t *testing.T) {
	for _, definition := range []string{
		"[model_providers.magpie]\n",
		"[model_providers.magpie]\nmax_request_bytes = 65000\n",
		"[model_providers]\nmagpie = { max_request_bytes = 65000 }\n",
		"model_providers.magpie = { max_request_bytes = 65000 }\n",
	} {
		t.Run(definition, func(t *testing.T) {
			home, path := grokSandbox(t)
			grokCatalogWrite(t, path, definition)
			a := grok(home)
			if err := a.Field("model").Set("magpie/deepseek/pro"); err != nil {
				t.Fatal(err)
			}
			if err := a.Field("model").Set(""); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(readFile(path), definition) {
				t.Fatalf("disconnect removed the user's provider definition:\n%s", readFile(path))
			}
		})
	}
}

func TestGrokCanonicalModelIDRoundTrips(t *testing.T) {
	home, path := grokSandbox(t)
	if err := provider.Save(provider.Provider{ID: "relay", Name: "Relay", Chat: "http://127.0.0.1:1/v1", Key: "k",
		Models: []string{"org/model:2026.11", `org/model "quoted"/x`, "org/model.name with space"}}); err != nil {
		t.Fatal(err)
	}
	a := grok(home)
	for _, want := range []string{"relay/org/model:2026.11", `relay/org/model "quoted"/x`, "relay/org/model.name with space"} {
		if err := a.Field("model").Set("magpie/" + want); err != nil {
			t.Fatal(err)
		}
		doc := grokCatalogConfig(t, path)
		if got := doc["models"].(map[string]any)["default"]; got != want {
			t.Fatalf("default ID changed: %v, want %q", got, want)
		}
		m, ok := doc["model"].(map[string]any)[want].(map[string]any)
		if !ok || m["model"] != want {
			t.Fatalf("registered ID changed:\n%s", readFile(path))
		}
	}
}

func TestGrokCanonicalIdentityLastsUntilRemovedModelIsReseated(t *testing.T) {
	home, path := grokSandbox(t)
	a := grok(home)
	if err := a.Field("model").Set("magpie/deepseek/pro"); err != nil {
		t.Fatal(err)
	}
	if err := provider.Save(provider.Provider{ID: "deepseek", Name: "DeepSeek", Chat: "http://127.0.0.1:1/v1", Key: "k",
		Models: []string{"flash"}}); err != nil {
		t.Fatal(err)
	}
	if err := a.Sync(); err != nil {
		t.Fatal(err)
	}
	if got := a.Field("model").Get(); got != "magpie/deepseek/pro" {
		t.Fatalf("a catalog refresh lost the selected identity before Reseat can compare it: %q", got)
	}
	if got := a.Check(); !strings.Contains(got, `[model."deepseek/pro"]`) || !strings.Contains(got, "gone") {
		t.Fatalf("the removed model stopped reporting its missing registration: %q\n%s", got, readFile(path))
	}
}

func TestGrokCanonicalKeepsUserWindowWithoutCatalogContext(t *testing.T) {
	home, path := grokSandbox(t)
	if err := provider.Save(provider.Provider{ID: "relay", Name: "Relay", Chat: "http://127.0.0.1:1/v1", Key: "k",
		Models: []string{"unknown-window"}}); err != nil {
		t.Fatal(err)
	}
	a := grok(home)
	if err := a.Field("model").Set("magpie/relay/unknown-window"); err != nil {
		t.Fatal(err)
	}
	if err := edit.SetTOMLKey(path, `model."relay/unknown-window"`, "context_window", 45000); err != nil {
		t.Fatal(err)
	}
	if err := a.Sync(); err != nil {
		t.Fatal(err)
	}
	m := grokCatalogConfig(t, path)["model"].(map[string]any)["relay/unknown-window"].(map[string]any)
	if m["context_window"] != int64(45000) {
		t.Fatalf("a user window was lost because the catalog has none: %v", m)
	}
}

func TestGrokCatalogReadFailureStopsEveryModelAction(t *testing.T) {
	for _, action := range []string{"set", "clear", "sync"} {
		t.Run(action, func(t *testing.T) {
			home, path := grokSandbox(t)
			if err := os.MkdirAll(path, 0o700); err != nil {
				t.Fatal(err)
			}
			a := grok(home)
			var err error
			switch action {
			case "set":
				err = a.Field("model").Set("magpie/deepseek/pro")
			case "clear":
				err = a.Field("model").Set("")
			case "sync":
				err = a.Sync()
			}
			if err == nil || !strings.Contains(err.Error(), path) {
				t.Fatalf("unreadable config was treated as empty: %v", err)
			}
		})
	}
}

func TestGrokCatalogDuplicateDefaultStopsClear(t *testing.T) {
	home, path := grokSandbox(t)
	const input = `[models]
default = "magpie/deepseek/pro"
default = "magpie/deepseek/flash"

[model."magpie/deepseek/pro"]
model = "deepseek/pro"
base_url = "http://127.0.0.1:3425/v1"
api_key = "magpie"
`
	grokCatalogWrite(t, path, input)
	if err := grok(home).Field("model").Set(""); err == nil {
		t.Fatal("a duplicate default was treated as a valid document")
	}
	if got := readFile(path); got != input {
		t.Fatalf("clear rewrote a document that did not parse:\n%s", got)
	}
}

func TestGrokCatalogDuplicateEffortStopsClear(t *testing.T) {
	home, path := grokSandbox(t)
	const input = `[models]
default_reasoning_effort = "high"
default_reasoning_effort = "low"
`
	grokCatalogWrite(t, path, input)
	if err := grok(home).Field("effort").Set(""); err == nil {
		t.Fatal("a duplicate reasoning default was treated as a valid document")
	}
	if got := readFile(path); got != input {
		t.Fatalf("clear rewrote a document that did not parse:\n%s", got)
	}
}

func grokCatalogConfig(t *testing.T, path string) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := toml.Unmarshal([]byte(readFile(path)), &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func grokCatalogWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func grokStringArray(value any) []string {
	var out []string
	values, _ := value.([]any)
	for _, v := range values {
		s, ok := v.(string)
		if !ok {
			return nil
		}
		out = append(out, s)
	}
	return out
}
