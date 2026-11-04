package agent

// Grok Build, xAI's grok CLI, keeps its settings in ~/.grok/config.toml (or
// $GROK_HOME's): the model new sessions start with under [models] default,
// and models of the user's own as [model."<id>"] tables. magpie adds one
// such table per catalog model, named by its public "<provider>/<model>" id,
// with model_provider = "magpie" identifying its ownership. Each points
// at the gateway with magpie's own key — a model with no key of its own
// would be sent the user's xAI sign-in — so the catalog joins Grok's own
// models in its /model picker.
//
// A signed-in Grok also takes remote "campaign" patches from xAI, applied
// above config.toml, and a launch campaign sets models.default (September
// 2026's grok-4.7-launch did): a default picked in magpie was ignored and
// every new session started on the campaign's model. So a default picked
// here turns campaigns off ([features] campaigns = false), and clearing it
// gives them back.

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
)

// grokEfforts are the reasoning efforts Grok knows.
var grokEfforts = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

const grokProviderTable = "model_providers." + magpieID

// grokModelTables are magpie's model tables, for a Grok that reaches the
// gateway's /v1 at v1, and the context_window magpie gives each model (by
// its catalog id), which the caller stashes. A context_window the user set
// in a table themselves (grokOwnWindow: had is what the tables hold now,
// wrote what magpie last gave them, nil before magpie kept a record) stays
// as they set it.
func grokModelTables(v1 string, models []catalog.Model, had, wrote map[string]int) ([]edit.Table, map[string]int) {
	var out []edit.Table
	st, gave := settings.Load(), map[string]int{}
	for _, m := range models {
		kvs := []edit.KV{
			{Path: "model", Value: m.ID},
			{Path: "model_provider", Value: magpieID},
			{Path: "name", Value: m.Name},
			{Path: "base_url", Value: v1},
			{Path: "api_key", Value: keyAt(v1)},
			{Path: "api_backend", Value: grokBackend(m)},
		}
		w := grokWindow(st, m)
		if w > 0 {
			gave[m.ID] = w
		}
		if n, ok := grokOwnWindow(m, w, had, wrote); ok {
			w = n
		}
		if w > 0 {
			kvs = append(kvs, edit.KV{Path: "context_window", Value: w})
		}
		var efforts []string
		for _, e := range grokEfforts { // in Grok's order
			if contains(m.Efforts, e) {
				efforts = append(efforts, strconv.Quote(e))
			}
		}
		if len(efforts) > 0 {
			kvs = append(kvs, edit.KV{Path: "reasoning_efforts", Value: edit.Raw("[" + strings.Join(efforts, ", ") + "]")})
		}
		out = append(out, edit.Table{Name: "model." + strconv.Quote(m.ID), KVs: kvs})
	}
	return out, gave
}

// grokWindow is the context_window Grok Build is told for m. Grok Build
// auto-compacts by it ("drives auto-compact timing", its config reference),
// so it is where Long conversations has Codex and Claude Code compact
// (codexcat's rule): the threshold set on the model or its provider, else
// settings' working window on a longer one, and the whole window under
// Full window (uk.jent on Discord: dozens of models told 1M, each set to
// 300K by hand). 0 when m's window isn't known.
func grokWindow(st settings.Settings, m catalog.Model) int {
	c := m.Context
	if c <= 0 {
		return 0
	}
	if m.Compact > 0 {
		return min(c, m.Compact)
	}
	return st.Working(c)
}

// grokOwnWindow is a context_window the user set in magpie's table for m
// themselves, which a sync keeps: one other than magpie last gave it. With
// no record of magpie's (wrote nil, a config an older magpie wrote, or a
// model new to the record), one other than the model's whole window, which
// an older magpie wrote, and other than w, which magpie gives it now.
func grokOwnWindow(m catalog.Model, w int, had, wrote map[string]int) (int, bool) {
	n, ok := had[m.ID]
	if !ok || n <= 0 || n == w {
		return 0, false
	}
	if prev, ok := wrote[m.ID]; ok {
		return n, n != prev
	}
	return n, n != m.Context
}

// grokBackend is the api_backend a model of magpie's is asked on: the API
// its provider serves it on natively, so the gateway relays what Grok sent
// as it is instead of translating Chat Completions. Grok Build takes
// "chat_completions" (its default), "responses" and "messages" (Anthropic
// Messages) on each [model.*] table, and sends each to base_url's
// /chat/completions, /responses or /messages, so the one /v1 serves all
// three. A model whose native API is Chat or not known stays on Chat.
func grokBackend(m catalog.Model) string {
	switch {
	case slices.Contains(m.APIs, string(provider.Responses)):
		return "responses"
	case slices.Contains(m.APIs, string(provider.Anthropic)):
		return "messages"
	}
	return "chat_completions"
}

func grok(home string) *Agent { return grokIn(here(home)) }

// grokIn is Grok Build at a place: this machine's home, or a WSL distro's
// (see wsl.go), where GROK_HOME isn't read and its models name the gateway
// as the distro reaches it.
func grokIn(at place) *Agent {
	dir := at.getenv("GROK_HOME")
	if dir == "" {
		dir = at.join(".grok")
	}
	path := filepath.Join(dir, "config.toml")
	get := func(k string) (string, error) {
		models, err := edit.GetTOMLTable(path, "models")
		return models[k], err
	}
	keyModels := "grok:" + path + ":models"
	keyProvider := "grok:" + path + ":provider"
	var a *Agent
	known := func() string {
		if a != nil {
			return appliedOf(a.ID).Fields["model"]
		}
		return ""
	}
	modelValue := func() string {
		v, err := get("default")
		if err != nil || v == "" || prefixed(v) {
			return v
		}
		t, err := grokModelIn(path, v)
		if err == nil && (t["model_provider"] == magpieID || t == nil && grokRecordedModel(path, v)) {
			return magpieID + "/" + v
		}
		return v
	}
	wired := func() (bool, error) {
		cfg, err := grokConfigIn(path, known())
		return len(cfg.owned) > 0, err
	}
	// keyWindows is the context_window magpie last gave each of its models,
	// so one the user set by hand since is told from it and kept
	keyWindows := "grok:" + path + ":windows"
	writeMagpie := func() error {
		cfg, err := grokConfigIn(path, known())
		if err != nil {
			return err
		}
		var wrote map[string]int
		saved := stashShared()[keyWindows]
		if saved != "" {
			json.Unmarshal([]byte(saved), &wrote)
		}
		models := sharedModels("grok")
		tables, gave := grokModelTables(at.v1(), models, cfg.windows(), wrote)
		ids := make([]string, 0, len(tables))
		for _, m := range models {
			if _, exists := cfg.models[m.ID]; exists && cfg.owned[m.ID] != m.ID {
				return grokModelCollision(path, m.ID)
			}
			ids = append(ids, m.ID)
		}
		def, err := get("default")
		if err != nil {
			return err
		}
		selected := cfg.owned[def]
		if selected == "" && cfg.models[def] == nil && grokRecordedModel(path, def) {
			selected = def
		}
		registered := slices.Clone(ids)
		if selected != "" && !slices.Contains(registered, selected) {
			// Keep the field's identity until Reseat moves it; dropping
			// its table must not make it look like a native Grok model.
			registered = append(registered, selected)
		}
		cache, err := grokPrunedCache(dir, cfg.aliases(ids))
		if err != nil {
			return err
		}
		// Every model carries its own endpoint and protocol. The named
		// provider is Grok's native ownership relationship; an empty
		// definition is sufficient, and existing user provider keys stay.
		if cfg.provider == "" {
			tables = append([]edit.Table{{Name: grokProviderTable}}, tables...)
		}
		if cfg.renamed != nil {
			if err := edit.WriteTOMLFile(path, edit.SplitLines(string(cfg.renamed))); err != nil {
				return err
			}
		}
		if err := edit.SetTOMLTablesMatching(path, cfg.names(), nil, tables); err != nil {
			return err
		}
		if cfg.owned[def] != "" && def != cfg.owned[def] {
			if err := edit.SetTOMLKey(path, "models", "default", cfg.owned[def]); err != nil {
				return err
			}
		}
		if cache != nil {
			if err := edit.WriteAtomic(filepath.Join(dir, "models_cache.json"), cache); err != nil {
				return err
			}
		}
		if cfg.provider == "" {
			stash(map[string]string{keyProvider: "1"})
		}
		b, _ := json.Marshal(registered)
		if stashShared()[keyModels] != string(b) {
			stash(map[string]string{keyModels: string(b)})
		}
		if !maps.Equal(gave, wrote) || saved == "" {
			b, _ := json.Marshal(gave)
			stash(map[string]string{keyWindows: string(b)})
		}
		return nil
	}
	dropMagpie := func() error {
		cfg, err := grokConfigIn(path, known())
		if err != nil {
			return err
		}
		cache, err := grokPrunedCache(dir, cfg.aliases(nil))
		if err != nil {
			return err
		}
		names := cfg.dropNames()
		if cfg.providerEmpty && stashShared()[keyProvider] == "1" {
			names = append(names, cfg.provider)
		}
		if err := edit.SetTOMLTablesMatching(path, names, nil, nil); err != nil {
			return err
		}
		if cache != nil {
			if err := edit.WriteAtomic(filepath.Join(dir, "models_cache.json"), cache); err != nil {
				return err
			}
		}
		forget(keyWindows, keyModels, keyProvider)
		return nil
	}
	// the default model is the user's, not a campaign's
	ownDefault := func(features map[string]string) error {
		if features["campaigns"] == "false" {
			return nil
		}
		return edit.SetTOMLKey(path, "features", "campaigns", false)
	}
	// the efforts of a model through magpie, as its catalog entry has them
	efforts := func(model string) []string {
		ref, ok := strings.CutPrefix(model, magpieID+"/")
		if !ok {
			return nil
		}
		return catalog.Efforts(magpieModels("grok"), ref)
	}
	a = &Agent{
		ID: "grok", Name: "Grok Build", Icon: "xai", Aliases: []string{"grok-build", "grok-cli"}, Spelled: prefixed,
		UA:  []string{"grok-shell", "grok-pager", "xai-grok-build"}, // grok-pager: its terminal front end
		Dir: dir, Path: path,
		Sync: func() error {
			ok, err := wired()
			if err != nil || !ok {
				return err
			}
			model, err := get("default")
			if err != nil {
				return err
			}
			if model != "" {
				features, err := edit.GetTOMLTable(path, "features")
				if err != nil {
					return err
				}
				if err := ownDefault(features); err != nil {
					return err
				}
			}
			return writeMagpie()
		},
		Notice: func() string {
			if Running(`(^|/)grok( |$)`) {
				return noticeGrok.String()
			}
			return ""
		},
		Check: func() string {
			v, err := get("default")
			if err != nil {
				return err.Error()
			}
			if !usesMagpie(modelValue()) {
				return ""
			}
			t, err := grokModelIn(path, v)
			if err != nil {
				return err.Error()
			}
			if t == nil {
				return "Grok Build's [model." + strconv.Quote(v) + "] (config.toml) is gone, so it no longer reaches magpie"
			}
			if t["model_provider"] == magpieID {
				cfg, err := grokConfigIn(path, known())
				if err != nil {
					return err.Error()
				}
				if cfg.provider == "" {
					return "Grok Build's [" + grokProviderTable + "] (config.toml) is gone, so it no longer reaches magpie"
				}
			}
			return wiringOff("Grok Build", path, func(k string) (string, bool) { v, ok := t[k]; return v, ok },
				"base_url", at.v1(), "api_key", at.gwKey())
		},
		Fields: []Field{
			{
				Key: "model", Label: "model",
				Get: modelValue,
				Set: func(v string) error {
					if _, err := grokConfigIn(path, known()); err != nil {
						return err
					}
					e, err := get("default_reasoning_effort")
					if err != nil {
						return err
					}
					features, err := edit.GetTOMLTable(path, "features")
					if err != nil {
						return err
					}
					if v == "" {
						if err := edit.DelTOMLKey(path, "models", "default"); err != nil {
							return err
						}
						if err := edit.DelTOMLKey(path, "features", "campaigns"); err != nil {
							return err
						}
						return dropMagpie()
					}
					if ref, ok := strings.CutPrefix(v, magpieID+"/"); ok && isMagpie(ref) {
						if err := writeMagpie(); err != nil {
							return err
						}
						v = ref
					} else if err := dropMagpie(); err != nil {
						return err
					}
					if e != "" {
						// a model without the chosen effort starts at its
						// nearest one (a tie goes up), not at Grok's default:
						// switching to a group of low/medium/high from xhigh
						// keeps high (Creat.wu on Discord)
						if es := efforts(magpieID + "/" + v); es != nil && !contains(es, e) {
							if n := catalog.NearestEffort(e, es); contains(es, n) {
								if err := edit.SetTOMLKey(path, "models", "default_reasoning_effort", n); err != nil {
									return err
								}
							} else if err := edit.DelTOMLKey(path, "models", "default_reasoning_effort"); err != nil {
								return err
							}
						}
					}
					if err := ownDefault(features); err != nil {
						return err
					}
					return edit.SetTOMLKey(path, "models", "default", v)
				},
				Options: func(cur map[string]string) []Option {
					// magpie's rows for the account Grok Build is signed in
					// to are its own models a second time: they fold into
					// one row (Fate on Discord: grokbuild 在登录态下会加载重复的模型)
					opts := viaMagpie("grok", magpieID+"/")
					for i := range opts {
						opts[i].Same = opts[i].own
					}
					return append(grokOwnOptions(dir, path, cur["model"]), opts...)
				},
			},
			{
				// the effort new sessions start with; Grok applies it to a
				// model that supports it and ignores it otherwise
				Key: "effort", Label: "effort",
				Get: func() string { v, _ := get("default_reasoning_effort"); return v },
				Set: func(v string) error {
					if _, err := grokConfigIn(path, known()); err != nil {
						return err
					}
					if v == "" {
						return edit.DelTOMLKey(path, "models", "default_reasoning_effort")
					}
					return edit.SetTOMLKey(path, "models", "default_reasoning_effort", v)
				},
				Options: func(cur map[string]string) []Option {
					if es := efforts(cur["model"]); es != nil {
						return static(es...)
					}
					return static("low", "medium", "high")
				},
			},
		},
	}
	return atomic(a, path, filepath.Join(dir, "models_cache.json"))
}

// grokOwnOptions are the models Grok Build offers its signed-in account: as
// it last listed them itself (models_cache.json in its home), and as magpie
// last listed them for a Grok subscription; then the models of the user's
// own config.toml tables, and the current one. Grok asks for them itself,
// on its sign-in or their own key, so each says so (Direct): a row not
// connected listed nothing of Grok's own, only magpie's, unless a Grok
// subscription was signed in in magpie (EZN7L2C3, #834).
func grokOwnOptions(dir, path, cur string) []Option {
	seen := map[string]bool{}
	managed := map[string]bool{}
	if path != "" {
		if cfg, err := grokConfigIn(path, ""); err == nil {
			for id := range cfg.owned {
				managed[id] = true
			}
		}
	}
	var out []Option
	add := func(id, name, direct string) {
		if id == "" || seen[id] || managed[id] || strings.HasPrefix(id, magpieID+"/") {
			return
		}
		seen[id] = true
		o := Option{Value: id, Icon: "xai", Direct: direct}
		if name != "" && name != id {
			o.Label = name
		}
		out = append(out, o)
	}
	// dir and path empty: a stopped distro's, whose files aren't read (#3896)
	if dir != "" {
		if raw, err := os.ReadFile(filepath.Join(dir, "models_cache.json")); err == nil {
			gjson.GetBytes(raw, "models").ForEach(func(k, v gjson.Result) bool {
				if !v.Get("info.hidden").Bool() {
					add(k.String(), v.Get("info.name").String(), "xAI")
				}
				return true
			})
		}
	}
	ms, _, _ := catalog.Live("grok")
	for _, m := range ms {
		add(m.ID, "", "xAI")
	}
	// the user's own [model."<id>"] tables, each on its own endpoint
	if path != "" {
		if tables, err := edit.TOMLTables(path); err == nil {
			for _, t := range tables {
				id, ok := strings.CutPrefix(t, "model.")
				if !ok {
					continue
				}
				if u, err := strconv.Unquote(id); err == nil {
					id = u
				}
				kv, _ := edit.GetTOMLTable(path, t)
				direct := "xAI"
				if h := hostOf(kv["base_url"]); h != "" {
					direct = h
				}
				add(id, kv["name"], direct)
			}
		}
	}
	if cur != "" && !seen[cur] && !strings.HasPrefix(cur, magpieID+"/") {
		out = append([]Option{{Value: cur, Icon: modelIcon("", cur), Direct: "xAI"}}, out...)
	}
	return group("Grok Build", out)
}

// what grok says after a change (notice.go)
var (
	noticeGrok = newNotice("Grok Build reads its settings at start-up — restart open grok sessions to use this.")
)
