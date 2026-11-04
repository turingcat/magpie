package agent

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/yetone/magpie/internal/appdir"
	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/provider"
	"gopkg.in/yaml.v3"
)

func hermesCatalogFixture(t *testing.T) (*Agent, string) {
	t.Helper()
	home := syncHome(t)
	if err := provider.Save(provider.Provider{
		ID: "source-a", Name: "Source A", Key: "test-key",
		Chat: "http://127.0.0.1:1/v1", Models: []string{"reasoner", "quiet"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.SaveLive("source-a", "http://127.0.0.1:1/v1", []catalog.Model{
		{ID: "reasoner", Name: "Reasoner", Context: 128000, Efforts: []string{"low", "high"}},
		{ID: "quiet", Name: "Quiet", Context: 64000},
	}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(appdir.HermesHomeIn(home), "config.yaml")
	writeFile(t, path, "# my config\nmodel:\n  provider: openrouter\n  default: native/model\n")
	a := hermes(home)
	if err := a.Field("model").Set("magpie/source-a/reasoner"); err != nil {
		t.Fatal(err)
	}
	return a, path
}

func readHermesModels(t *testing.T, path string) map[string]map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Providers map[string]struct {
			Models any `yaml:"models"`
		} `yaml:"providers"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	entries, ok := doc.Providers["magpie"].Models.(map[string]any)
	if !ok {
		t.Fatalf("Hermes needs per-model metadata keyed by callable ID, got %T", doc.Providers["magpie"].Models)
	}
	out := make(map[string]map[string]any, len(entries))
	for id, value := range entries {
		out[id], ok = value.(map[string]any)
		if !ok {
			t.Fatalf("model %s has no metadata: %v", id, value)
		}
	}
	return out
}

func TestHermesCatalogCarriesUpstreamAndEfforts(t *testing.T) {
	a, path := hermesCatalogFixture(t)
	models := readHermesModels(t, path)
	reasoner, ok := models["source-a/reasoner"]
	if !ok || reasoner["group"] != "source-a" || reasoner["context_length"] != 128000 {
		t.Fatalf("reasoning model metadata: %v", reasoner)
	}
	if name, ok := reasoner["name"].(string); !ok || name == "" ||
		!reflect.DeepEqual(reasoner["reasoning_efforts"], []any{"low", "high"}) {
		t.Fatalf("name and real supported efforts: %v", reasoner)
	}
	if quiet := models["source-a/quiet"]; quiet["group"] != "source-a" ||
		!reflect.DeepEqual(quiet["reasoning_efforts"], []any{}) {
		t.Fatalf("nonreasoning model must not invent levels: %v", quiet)
	}
	if got := a.Field("model").Get(); got != "magpie/source-a/reasoner" {
		t.Fatalf("the callable model changed: %s", got)
	}
	if got, _ := edit.GetYAML(path, "model.default"); got != "source-a/reasoner" {
		t.Fatalf("Hermes runtime model changed: %s", got)
	}
}

func TestHermesCatalogGroupsPublicRoutingAndProviderIDs(t *testing.T) {
	a, path := hermesCatalogFixture(t)
	if err := provider.Save(provider.Provider{
		ID: "source-b", Name: "Source B", Key: "test-key",
		Chat: "http://127.0.0.1:1/v1", Models: []string{"reasoner"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.SaveLive("source-b", "http://127.0.0.1:1/v1", []catalog.Model{
		{ID: "reasoner", Name: "Reasoner", Context: 128000, Efforts: []string{"low", "high"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := provider.SaveGroup(provider.Group{
		ID: "daily", Name: "Daily", Members: []string{"source-a/reasoner", "source-b/reasoner"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.Sync(); err != nil {
		t.Fatal(err)
	}
	models := readHermesModels(t, path)
	for id, group := range map[string]string{
		"source-a/reasoner": "source-a",
		"source-b/reasoner": "source-b",
		"auto/reasoner":     "auto",
		"group/daily":       "group",
	} {
		if got := models[id]["group"]; got != group {
			t.Errorf("%s has source group %v, want %s", id, got, group)
		}
	}
}

func TestHermesCatalogSyncKeepsUserModelOverrides(t *testing.T) {
	a, path := hermesCatalogFixture(t)
	models := readHermesModels(t, path)
	models["source-a/reasoner"]["context_length"] = 96000
	models["source-a/reasoner"]["reasoning_efforts"] = []string{"high"}
	models["source-a/reasoner"]["description"] = "My internal model"
	delete(models["source-a/reasoner"], "name")
	models["source-a/quiet"]["description"] = "Removed with the catalog entry"
	if err := edit.SetYAML(path, edit.KV{Path: "providers.magpie.models", Value: models}); err != nil {
		t.Fatal(err)
	}
	if err := provider.Save(provider.Provider{
		ID: "source-a", Name: "Source A", Key: "test-key",
		Chat: "http://127.0.0.1:1/v1", Models: []string{"reasoner"},
		Contexts: map[string]int{"reasoner": 256000},
	}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := a.Sync(); err != nil {
			t.Fatal(err)
		}
	}
	models = readHermesModels(t, path)
	reasoner := models["source-a/reasoner"]
	if reasoner["context_length"] != 96000 || reasoner["description"] != "My internal model" ||
		!reflect.DeepEqual(reasoner["reasoning_efforts"], []any{"high"}) {
		t.Fatalf("sync overwrote user model settings: %v", reasoner)
	}
	if _, restored := reasoner["name"]; restored {
		t.Fatal("sync restored a model name the user removed")
	}
	if _, stale := models["source-a/quiet"]; stale {
		t.Fatal("a removed model stayed in the generated catalog")
	}
}

func TestHermesCatalogRefreshesGeneratedCapabilities(t *testing.T) {
	a, path := hermesCatalogFixture(t)
	models := readHermesModels(t, path)
	models["source-a/reasoner"]["description"] = "Keep this note"
	if err := edit.SetYAML(path, edit.KV{Path: "providers.magpie.models", Value: models}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.SaveLive("source-a", "http://127.0.0.1:1/v1", []catalog.Model{
		{ID: "reasoner", Name: "Reasoner", Context: 256000, Efforts: []string{"low", "medium", "high"}},
		{ID: "quiet", Name: "Quiet", Context: 64000},
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.Sync(); err != nil {
		t.Fatal(err)
	}
	got := readHermesModels(t, path)["source-a/reasoner"]
	if got["context_length"] != 256000 ||
		!reflect.DeepEqual(got["reasoning_efforts"], []any{"low", "medium", "high"}) ||
		got["description"] != "Keep this note" {
		t.Fatalf("catalog refresh did not update only the generated capabilities: %v", got)
	}
}

func TestHermesCatalogUnreadableRecordKeepsUserSettings(t *testing.T) {
	a, path := hermesCatalogFixture(t)
	models := readHermesModels(t, path)
	models["source-a/reasoner"]["context_length"] = 96000
	if err := edit.SetYAML(path, edit.KV{Path: "providers.magpie.models", Value: models}); err != nil {
		t.Fatal(err)
	}
	// A structurally invalid record can decode a valid prefix before returning
	// an error. That prefix must not claim the user's current value as magpie's.
	stash(map[string]string{
		"hermes:" + path + ":catalog": `{"source-a/reasoner":{"context_length":96000},"broken":"not a model mapping"}`,
	})
	if err := a.Sync(); err != nil {
		t.Fatal(err)
	}
	if got := readHermesModels(t, path)["source-a/reasoner"]["context_length"]; got != 96000 {
		t.Fatalf("an unreadable ownership record overwrote the user's context: %v", got)
	}
}
