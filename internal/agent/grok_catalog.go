package agent

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
	"github.com/pelletier/go-toml/v2/unstable"

	"github.com/yetone/magpie/internal/edit"
)

// grokConfig identifies actual ordinary model tables, retaining their exact
// TOML header spellings so a user table sharing a prefix is never removed.
type grokConfig struct {
	models        map[string]any
	headers       map[string]string
	owned         map[string]string // registered id -> public catalog id
	children      []string
	reasoning     []string
	renamed       []byte
	provider      string
	providerEmpty bool
}

func grokConfigIn(path, known string) (grokConfig, error) {
	cfg := grokConfig{headers: map[string]string{}, owned: map[string]string{}}
	headers, err := edit.TOMLTables(path)
	if err != nil {
		return cfg, err
	}
	raw, err := edit.Read(path)
	if err != nil || raw == nil {
		return cfg, err
	}
	var doc map[string]any
	if err := toml.Unmarshal(raw, &doc); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	cfg.models, _ = doc["model"].(map[string]any)
	providers, _ := doc["model_providers"].(map[string]any)
	if _, ok := providers[magpieID].(map[string]any); ok {
		// Inline and dotted definitions supply the relationship too.
		// Only an explicit empty table can be removed by exact name.
		cfg.provider = grokProviderTable
	}
	for _, header := range headers {
		if id, ok := grokTableID(header, "model_providers"); ok && id == magpieID {
			cfg.provider = header
			p, ok := providers[id].(map[string]any)
			cfg.providerEmpty = ok && len(p) == 0
		}
		id, ok := grokTableID(header, "model")
		if !ok {
			continue
		}
		cfg.headers[id] = header
		m, ok := cfg.models[id].(map[string]any)
		if !ok {
			continue
		}
		if m["model_provider"] == magpieID {
			cfg.owned[id] = id
			continue
		}
		// Old Magpie tables had no provider relationship. Their alias,
		// wire model and gateway credential (or Magpie's setup record)
		// must agree before they can be migrated.
		ref, legacy := strings.CutPrefix(id, magpieID+"/")
		key, _ := m["api_key"].(string)
		if legacy && ref != "" && m["model"] == ref &&
			(known == id || gatewayKey(key) || grokRecordedModel(path, ref)) {
			cfg.owned[id] = ref
		}
	}
	// Child table headers have their own spelling, possibly different
	// quotes from the parent. Match their parsed identity, then keep exact
	// names for removal. User subtables follow a legacy model's rename.
	type replacement struct {
		from, to int
		value    string
	}
	var replacements []replacement
	var parser unstable.Parser
	parser.Reset(raw)
	for parser.NextExpression() {
		expr := parser.Expression()
		if expr.Kind != unstable.Table && expr.Kind != unstable.ArrayTable {
			continue
		}
		var parts, spelled []string
		var modelKey unstable.Range
		for keys := expr.Key(); keys.Next(); {
			key := keys.Node()
			parts = append(parts, string(key.Data))
			spelled = append(spelled, string(parser.Raw(key.Raw)))
			if len(parts) == 2 {
				modelKey = key.Raw
			}
		}
		if len(parts) < 3 || parts[0] != "model" {
			continue
		}
		ref, owned := cfg.owned[parts[1]]
		if !owned {
			continue
		}
		name := strings.Join(spelled, ".")
		cfg.children = append(cfg.children, name)
		if parts[2] == "reasoning_efforts" {
			cfg.reasoning = append(cfg.reasoning, name)
		} else if ref != parts[1] {
			replacements = append(replacements, replacement{
				int(modelKey.Offset), int(modelKey.Offset + modelKey.Length), strconv.Quote(ref),
			})
		}
	}
	if err := parser.Error(); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	if len(replacements) > 0 {
		var out strings.Builder
		at := 0
		for _, r := range replacements {
			out.Write(raw[at:r.from])
			out.WriteString(r.value)
			at = r.to
		}
		out.Write(raw[at:])
		cfg.renamed = []byte(out.String())
	}
	return cfg, nil
}

// grokTableID accepts exactly one model/provider table. Parsing the header
// also accepts literal quotes and escaped IDs without confusing child tables
// or text inside multiline values with another model.
func grokTableID(header, parent string) (string, bool) {
	if part, ok := strings.CutPrefix(header, parent+"."); ok {
		if id, err := strconv.Unquote(part); err == nil {
			return id, true
		}
	}
	var doc map[string]map[string]map[string]any
	if err := toml.Unmarshal([]byte("["+header+"]\n"), &doc); err != nil {
		return "", false
	}
	entries := doc[parent]
	if len(entries) != 1 {
		return "", false
	}
	for id, values := range entries {
		return id, len(values) == 0
	}
	return "", false
}

func grokModelIn(path, id string) (map[string]string, error) {
	headers, err := edit.TOMLTables(path)
	if err != nil {
		return nil, err
	}
	for _, header := range headers {
		if model, ok := grokTableID(header, "model"); ok && model == id {
			return edit.GetTOMLTable(path, header)
		}
	}
	return nil, nil
}

func (cfg grokConfig) names() []string {
	var out []string
	for id := range cfg.owned {
		out = append(out, cfg.headers[id])
	}
	out = append(out, cfg.reasoning...)
	slices.Sort(out)
	return out
}

func (cfg grokConfig) dropNames() []string {
	out := append(cfg.names(), cfg.children...)
	slices.Sort(out)
	return slices.Compact(out)
}

func (cfg grokConfig) windows() map[string]int {
	out := map[string]int{}
	for id, ref := range cfg.owned {
		// A current canonical table wins over a legacy duplicate.
		if id != ref && cfg.owned[ref] == ref {
			continue
		}
		m := cfg.models[id].(map[string]any)
		if n, ok := m["context_window"].(int64); ok {
			out[ref] = int(n)
		}
	}
	return out
}

func grokRecordedModel(path, id string) bool {
	var ids []string
	json.Unmarshal([]byte(stashShared()["grok:"+path+":models"]), &ids)
	if slices.Contains(ids, id) {
		return true
	}
	var windows map[string]int
	json.Unmarshal([]byte(stashShared()["grok:"+path+":windows"]), &windows)
	_, ok := windows[id]
	return ok
}

func grokModelCollision(path, id string) error {
	return fmt.Errorf("%s: Grok Build's [model.%s] is already configured by the user; rename that table before connecting magpie", path, strconv.Quote(id))
}

func (cfg grokConfig) aliases(ids []string) map[string]bool {
	out := map[string]bool{}
	for _, ref := range cfg.owned {
		out[magpieID+"/"+ref] = true
	}
	for _, id := range ids {
		out[magpieID+"/"+id] = true
	}
	for alias := range out {
		if _, exists := cfg.models[alias]; exists && cfg.owned[alias] == "" {
			delete(out, alias)
		}
	}
	return out
}

// grokPrunedCache removes only aliases belonging to a known Magpie model.
// It keeps native models, canonical entries and cache metadata intact. A bad
// read or parse stops the migration instead of replacing an unknown cache.
func grokPrunedCache(dir string, aliases map[string]bool) ([]byte, error) {
	if len(aliases) == 0 {
		return nil, nil
	}
	path := filepath.Join(dir, "models_cache.json")
	raw, err := edit.Read(path)
	if err != nil || raw == nil {
		return nil, err
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var models map[string]json.RawMessage
	if err := json.Unmarshal(doc["models"], &models); err != nil {
		return nil, fmt.Errorf("%s: models: %w", path, err)
	}
	changed := false
	for id := range aliases {
		if _, ok := models[id]; ok {
			delete(models, id)
			changed = true
		}
	}
	if !changed {
		return nil, nil
	}
	doc["models"], _ = json.Marshal(models)
	return json.MarshalIndent(doc, "", "  ")
}
