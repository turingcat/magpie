package agent

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
)

func catalogHermesLive(t *testing.T, context int, efforts ...string) {
	t.Helper()
	if efforts == nil {
		efforts = []string{"low", "high"}
	}
	if err := catalog.SaveLive("source-a", "http://127.0.0.1:1/v1", []catalog.Model{
		{ID: "reasoner", Name: "Reasoner", Context: context, Efforts: efforts},
		{ID: "quiet", Name: "Quiet", Context: 64000},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestHermesCatalogUserOverrideStaysOwnedWhenGeneratedValueMatches(t *testing.T) {
	for _, record := range []string{"present", "missing", "unreadable"} {
		t.Run(record, func(t *testing.T) {
			a, path := hermesCatalogFixture(t)
			models := readHermesModels(t, path)
			models["source-a/reasoner"]["context_length"] = 96000
			if err := edit.SetYAML(path, edit.KV{Path: "providers.magpie.models", Value: models}); err != nil {
				t.Fatal(err)
			}
			key := "hermes:" + path + ":catalog"
			switch record {
			case "missing":
				forget(key)
			case "unreadable":
				stash(map[string]string{key: `{"source-a/reasoner":{"context_length":96000},"broken":false}`})
			}
			for _, context := range []int{96000, 256000} {
				catalogHermesLive(t, context)
				if err := a.Sync(); err != nil {
					t.Fatal(err)
				}
			}
			if got := readHermesModels(t, path)["source-a/reasoner"]["context_length"]; got != 96000 {
				t.Fatalf("a future catalog equality reclaimed the user's 96000 override: %v", got)
			}
		})
	}
}

func TestHermesCatalogDeletionSurvivesAbsentGeneratedMetadata(t *testing.T) {
	for _, record := range []string{"present", "missing"} {
		t.Run(record, func(t *testing.T) {
			a, path := hermesCatalogFixture(t)
			models := readHermesModels(t, path)
			delete(models["source-a/reasoner"], "context_length")
			if err := edit.SetYAML(path, edit.KV{Path: "providers.magpie.models", Value: models}); err != nil {
				t.Fatal(err)
			}
			if record == "missing" {
				forget("hermes:" + path + ":catalog")
			}
			for _, context := range []int{0, 256000} {
				catalogHermesLive(t, context)
				if err := a.Sync(); err != nil {
					t.Fatal(err)
				}
			}
			if got, present := readHermesModels(t, path)["source-a/reasoner"]["context_length"]; present {
				t.Fatalf("a deleted context returned after the catalog omitted it: %v", got)
			}
		})
	}
}

func TestGrokUserWindowStaysOwnedWhenGeneratedValueMatches(t *testing.T) {
	home, path := grokSandbox(t)
	a := grok(home)
	if err := a.Field("model").Set("magpie/deepseek/pro"); err != nil {
		t.Fatal(err)
	}
	if err := edit.SetTOMLKey(path, `model."deepseek/pro"`, "context_window", 96000); err != nil {
		t.Fatal(err)
	}
	for _, context := range []int{96000, 256000} {
		s := settings.Load()
		s.CompactAt = context
		if err := settings.Save(s); err != nil {
			t.Fatal(err)
		}
		if err := a.Sync(); err != nil {
			t.Fatal(err)
		}
	}
	if pro, _ := grokWindows(t, path); pro != 96000 {
		t.Fatalf("a future generated equality reclaimed the user's Grok window: %d", pro)
	}
}

func TestGrokDeletedWindowSurvivesAbsentCatalogContext(t *testing.T) {
	home, path := grokSandbox(t)
	a := grok(home)
	if err := a.Field("model").Set("magpie/deepseek/pro"); err != nil {
		t.Fatal(err)
	}
	if err := edit.DelTOMLKey(path, `model."deepseek/pro"`, "context_window"); err != nil {
		t.Fatal(err)
	}
	for _, context := range []int{0, 256000} {
		if err := provider.Save(provider.Provider{ID: "deepseek", Name: "DeepSeek", Key: "k",
			Chat: "http://127.0.0.1:1/v1", Models: []string{"pro", "flash"},
			Contexts: map[string]int{"pro": context, "flash": 200000}}); err != nil {
			t.Fatal(err)
		}
		if err := a.Sync(); err != nil {
			t.Fatal(err)
		}
	}
	m := grokCatalogConfig(t, path)["model"].(map[string]any)["deepseek/pro"].(map[string]any)
	if value, present := m["context_window"]; present {
		t.Fatalf("a deleted Grok window returned after the catalog omitted it: %v", value)
	}
}

// Fail the selected write by temporarily putting an empty directory at its
// target. Restore it before an atomic rollback's next write, or on return for
// a caller that has no rollback. This works without permission-bit semantics.
func catalogFailWrite(t *testing.T, path string, match func([]byte) bool, before func(), run func() error) error {
	t.Helper()
	target, err := edit.Target(path)
	if err != nil {
		t.Fatal(err)
	}
	saved := target + ".failed-catalog-write"
	fired, blocked := false, false
	restore := func() {
		t.Helper()
		if !blocked {
			return
		}
		if err := os.Remove(target); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(saved, target); err != nil {
			t.Fatal(err)
		}
		blocked = false
	}
	defer restore()
	err = edit.Filtered(func(p string, data []byte) []byte {
		if p != target {
			return data
		}
		if blocked {
			restore()
		} else if !fired && match(data) {
			if before != nil {
				before()
			}
			if err := os.Rename(target, saved); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(target, 0o700); err != nil {
				t.Fatal(err)
			}
			fired, blocked = true, true
		}
		return data
	}, run)
	if !fired || err == nil {
		t.Fatalf("the selected config write did not fail: injected %v, error %v", fired, err)
	}
	return err
}

func TestHermesCatalogFailedWriteDoesNotAdvanceOwnershipOrFreezeRetry(t *testing.T) {
	a, path := hermesCatalogFixture(t)
	key := "hermes:" + path + ":catalog"
	before := stashShared()[key]
	catalogHermesLive(t, 256000)
	catalogFailWrite(t, path, func([]byte) bool { return true }, nil, a.Sync)
	if got := stashShared()[key]; got != before {
		t.Error("a failed config write advanced its catalog ownership")
	}
	if err := a.Sync(); err != nil {
		t.Fatal(err)
	}
	if got := readHermesModels(t, path)["source-a/reasoner"]["context_length"]; got != 256000 {
		t.Fatalf("retry treated untouched old generated metadata as a user override: %v", got)
	}
}

func TestHermesCatalogFailedProfileSyncKeepsEachRecordMatchedToItsConfig(t *testing.T) {
	a, path := hermesCatalogFixture(t)
	profile := filepath.Join(filepath.Dir(path), "profiles", "bot", "config.yaml")
	writeFile(t, profile, "model:\n  provider: openrouter\n  default: native/bot\n")
	if err := a.Sync(); err != nil {
		t.Fatal(err)
	}
	beforeConfig, beforeProfile := readFile(path), readFile(profile)
	beforeRecord, beforeBot := stashShared()["hermes:"+path+":catalog"], stashShared()["hermes:"+profile+":catalog"]
	catalogHermesLive(t, 256000)
	catalogFailWrite(t, profile, func([]byte) bool { return true }, nil, a.Sync)
	// Either per-file commit or a complete rollback is valid. This assertion
	// catches ownership changes whose matching config did not change.
	if readFile(path) == beforeConfig && stashShared()["hermes:"+path+":catalog"] != beforeRecord {
		t.Error("the root record advanced while its config rolled back")
	}
	if readFile(profile) == beforeProfile && stashShared()["hermes:"+profile+":catalog"] != beforeBot {
		t.Error("the failed profile record advanced without its config")
	}
	if err := a.Sync(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, profile} {
		if got := readHermesModels(t, p)["source-a/reasoner"]["context_length"]; got != 256000 {
			t.Errorf("successful retry froze generated metadata in %s: %v", p, got)
		}
	}
}

func TestHermesCatalogFailedConnectDoesNotClaimProfileMetadata(t *testing.T) {
	_, path := hermesCatalogFixture(t)
	if err := edit.DelYAML(path, "providers.magpie"); err != nil {
		t.Fatal(err)
	}
	if err := edit.SetYAML(path, edit.KV{Path: "model.provider", Value: "openrouter"},
		edit.KV{Path: "model.default", Value: "native/root"}); err != nil {
		t.Fatal(err)
	}
	forget("hermes:" + path + ":catalog")
	profile := filepath.Join(filepath.Dir(path), "profiles", "bot", "config.yaml")
	writeFile(t, profile, "model:\n  provider: openrouter\n  default: native/bot\n")
	beforeRoot, beforeBot := readFile(path), readFile(profile)
	a := hermes(os.Getenv("HOME"))
	catalogFailWrite(t, profile, func([]byte) bool { return true }, nil,
		func() error { return a.Field("model").Set("magpie/source-a/reasoner") })
	if readFile(path) != beforeRoot || readFile(profile) != beforeBot {
		t.Fatal("a late profile failure left a partially connected config")
	}
	if stashShared()["hermes:"+path+":catalog"] != "" || stashShared()["hermes:"+profile+":catalog"] != "" {
		t.Fatal("a failed connect claimed metadata for configs that rolled back")
	}
}

func TestHermesCatalogProfileSetFailureDoesNotAdvanceOwnership(t *testing.T) {
	_, path := hermesCatalogFixture(t)
	profile := filepath.Join(filepath.Dir(path), "profiles", "bot", "config.yaml")
	writeFile(t, profile, "model:\n  provider: openrouter\n  default: native/bot\n")
	a := hermes(os.Getenv("HOME"))
	f := a.Field("profile:bot")
	if f == nil {
		t.Fatal("the real profile field was not created")
	}
	before := readFile(profile)
	catalogFailWrite(t, profile, func([]byte) bool { return true }, nil,
		func() error { return f.Set("magpie/source-a/reasoner") })
	if readFile(profile) != before || stashShared()["hermes:"+profile+":catalog"] != "" {
		t.Fatal("a failed profile Set advanced metadata ownership")
	}
	if err := f.Set("magpie/source-a/reasoner"); err != nil {
		t.Fatal(err)
	}
	if got := readHermesModels(t, profile)["source-a/reasoner"]["context_length"]; got != 128000 {
		t.Fatalf("profile retry did not export its generated catalog: %v", got)
	}
}

func TestHermesCatalogOuterStashTypeErrorClaimsNoValues(t *testing.T) {
	a, path := hermesCatalogFixture(t)
	data, err := json.Marshal(stashShared())
	if err != nil {
		t.Fatal(err)
	}
	broken := string(data[:len(data)-1]) + `, "broken-value": 42}`
	if err := os.WriteFile(stashPath(), []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}
	catalogHermesLive(t, 256000)
	if err := a.Sync(); err != nil {
		t.Fatal(err)
	}
	if got := readHermesModels(t, path)["source-a/reasoner"]["context_length"]; got != 128000 {
		t.Fatalf("partially decoded outer stash claimed an existing value: %v", got)
	}
}

func TestStashOuterTypeErrorDiscardsAllValuesAndKeepsBadCopy(t *testing.T) {
	stashHome(t)
	path := stashPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	const broken = `{"agent.was":"old","broken":42,"tail.was":"also old"}`
	if err := os.WriteFile(path, []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := stashShared(); len(got) != 0 {
		t.Fatalf("an outer decode error exposed a partial trusted stash: %v", got)
	}
	stash(map[string]string{"next.was": "new"})
	if got := stashLoad(); len(got) != 1 || got["next.was"] != "new" {
		t.Fatalf("a recovery write kept partially decoded ownership: %v", got)
	}
	bad, err := filepath.Glob(path + ".bad-*")
	if err != nil || len(bad) != 1 || readFile(bad[0]) != broken {
		t.Fatalf("the original bad stash was not retained: %v, %v", bad, err)
	}
}

func TestGrokRemovedModelChildrenCannotOutliveClear(t *testing.T) {
	home, path := grokSandbox(t)
	a := grok(home)
	if err := a.Field("model").Set("magpie/deepseek/pro"); err != nil {
		t.Fatal(err)
	}
	if err := edit.SetTOMLTable(path, `model."deepseek/pro".extra_headers`,
		edit.KV{Path: "X-Proxy-Route", Value: "keep"}); err != nil {
		t.Fatal(err)
	}
	if err := provider.Save(provider.Provider{ID: "deepseek", Name: "DeepSeek",
		Chat: "http://127.0.0.1:1/v1", Key: "k", Models: []string{"flash"}}); err != nil {
		t.Fatal(err)
	}
	if err := a.Sync(); err != nil {
		t.Fatal(err)
	}
	models, _ := grokCatalogConfig(t, path)["model"].(map[string]any)
	if models["deepseek/pro"] != nil {
		t.Errorf("a removed model was recreated by a retained child table: %v", models["deepseek/pro"])
	}
	if err := a.Field("model").Set(""); err != nil {
		t.Fatal(err)
	}
	models, _ = grokCatalogConfig(t, path)["model"].(map[string]any)
	if models["deepseek/pro"] != nil {
		t.Fatalf("a removed model's implicit subtree survived Clear: %v", models["deepseek/pro"])
	}
}

func TestGrokFailedSetDefersAllOwnershipAndKeepsConcurrentStash(t *testing.T) {
	home, path := grokSandbox(t)
	const input = "[models]\ndefault = \"grok-native\"\n\n[features]\ncampaigns = true\n"
	grokCatalogWrite(t, path, input)
	cache := filepath.Join(filepath.Dir(path), "models_cache.json")
	const cached = `{"models":{"magpie/deepseek/pro":{"info":{"id":"magpie/deepseek/pro"}}}}`
	grokCatalogWrite(t, cache, cached)
	a := grok(home)
	catalogFailWrite(t, path, func(data []byte) bool {
		return bytes.Contains(data, []byte("default = \"deepseek/pro\""))
	}, func() { stash(map[string]string{"another-agent.was": "keep"}) },
		func() error { return a.Field("model").Set("magpie/deepseek/pro") })
	if readFile(path) != input || readFile(cache) != cached {
		t.Fatal("a failed Set did not restore its config and cache")
	}
	for _, suffix := range []string{"provider", "models", "windows"} {
		if got := stashShared()["grok:"+path+":"+suffix]; got != "" {
			t.Errorf("a failed Set committed %s ownership: %s", suffix, got)
		}
	}
	if stashShared()["another-agent.was"] != "keep" {
		t.Fatal("rollback erased another agent's concurrent stash update")
	}
	if err := edit.SetTOMLTable(path, grokProviderTable); err != nil {
		t.Fatal(err)
	}
	if err := a.Field("model").Set("magpie/deepseek/pro"); err != nil {
		t.Fatal(err)
	}
	if err := a.Field("model").Set(""); err != nil {
		t.Fatal(err)
	}
	providers, _ := grokCatalogConfig(t, path)["model_providers"].(map[string]any)
	if _, exists := providers["magpie"]; !exists {
		t.Fatal("an abandoned failed-Set flag later removed the user's provider")
	}
}

func TestGrokFailedNativeSetKeepsRolledBackOwnership(t *testing.T) {
	home, path := grokSandbox(t)
	a := grok(home)
	if err := a.Field("model").Set("magpie/deepseek/pro"); err != nil {
		t.Fatal(err)
	}
	before := readFile(path)
	records := map[string]string{}
	for _, suffix := range []string{"provider", "models", "windows"} {
		records[suffix] = stashShared()["grok:"+path+":"+suffix]
	}
	catalogFailWrite(t, path, func(data []byte) bool {
		return bytes.Contains(data, []byte("default = \"grok-native\""))
	}, nil, func() error { return a.Field("model").Set("grok-native") })
	if readFile(path) != before {
		t.Fatal("a failed native Set did not restore the managed config")
	}
	for suffix, want := range records {
		if got := stashShared()["grok:"+path+":"+suffix]; got != want {
			t.Errorf("failed native Set forgot ownership of a restored %s record", suffix)
		}
	}
}

func TestGrokModelChoicesExcludeAllChildHeaders(t *testing.T) {
	home, path := grokSandbox(t)
	a := grok(home)
	if err := a.Field("model").Set("magpie/deepseek/pro"); err != nil {
		t.Fatal(err)
	}
	if err := edit.SetTOMLTable(path, `model."deepseek/pro".extra_headers`,
		edit.KV{Path: "X-Proxy-Route", Value: "keep"}); err != nil {
		t.Fatal(err)
	}
	if err := edit.SetTOMLTable(path, `model.'my.native/model'`,
		edit.KV{Path: "model", Value: "my-model"}, edit.KV{Path: "base_url", Value: "https://native.example/v1"}); err != nil {
		t.Fatal(err)
	}
	var native bool
	for _, o := range a.Field("model").Options(a.Values()) {
		if strings.Contains(o.Value, ".extra_headers") {
			t.Fatalf("a child header was exposed as a callable model: %+v", o)
		}
		native = native || o.Value == "my.native/model" && o.Direct == "native.example"
	}
	if !native {
		t.Fatal("a valid literal-quoted ordinary model was not enumerated")
	}
}

func TestHermesCatalogNormalizesOffWithoutInventingLevels(t *testing.T) {
	a, path := hermesCatalogFixture(t)
	catalogHermesLive(t, 128000, "off", "none", "high")
	if err := a.Sync(); err != nil {
		t.Fatal(err)
	}
	models := readHermesModels(t, path)
	if got := models["source-a/reasoner"]["reasoning_efforts"]; !reflect.DeepEqual(got, []any{"none", "high"}) {
		t.Fatalf("Hermes did not receive one native reasoning-off choice: %v", got)
	}
	if got, present := models["source-a/quiet"]["reasoning_efforts"]; !present || !reflect.DeepEqual(got, []any{}) {
		t.Fatalf("an unknown/empty effort list must stay explicitly empty: present %v, %v", present, got)
	}
}

func TestGrokRemovedAliasKeepsUnownedCanonicalReasoning(t *testing.T) {
	home, path := grokSandbox(t)
	const input = `[models]
default = "magpie/deepseek/pro"

[model."magpie/deepseek/pro"]
model = "deepseek/pro"
api_key = "magpie"

[[model."magpie/deepseek/pro".reasoning_efforts]]
value = "high"
label = "Old managed High"

[model."deepseek/pro"]
model = "my-private-model"
base_url = "https://native.example/v1"
api_key = "user-private-key"

[[model."deepseek/pro".reasoning_efforts]]
value = "max"
label = "My Max"
`
	grokCatalogWrite(t, path, input)
	if err := provider.Save(provider.Provider{ID: "deepseek", Name: "DeepSeek",
		Chat: "http://127.0.0.1:1/v1", Key: "k", Models: []string{"flash"}}); err != nil {
		t.Fatal(err)
	}
	err := grok(home).Sync()
	// Refusing a collision is valid too; either result must preserve the
	// complete unowned table, including the user's reasoning metadata.
	models, _ := grokCatalogConfig(t, path)["model"].(map[string]any)
	model, _ := models["deepseek/pro"].(map[string]any)
	levels, ok := model["reasoning_efforts"].([]any)
	if !ok || len(levels) != 1 {
		t.Fatalf("removed alias cleanup deleted unowned reasoning metadata (sync error %v): %v", err, model)
	}
	level, _ := levels[0].(map[string]any)
	if level["value"] != "max" || level["label"] != "My Max" ||
		model["model"] != "my-private-model" || model["base_url"] != "https://native.example/v1" || model["api_key"] != "user-private-key" {
		t.Fatalf("removed alias cleanup changed the unowned canonical model (sync error %v): %v", err, model)
	}
}
