package agent

import (
	"bytes"
	"encoding/json"
	"strings"
)

// hermesModels gives Hermes explicit per-model metadata without changing its
// callable IDs. group is the public upstream prefix, not a models.dev maker.
// An empty reasoning_efforts list explicitly offers no thinking choices.
func hermesModels() map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, m := range sharedModels("hermes") {
		group, _, _ := strings.Cut(m.ID, "/")
		entry := map[string]any{
			"name": m.Name, "group": group,
			"reasoning_efforts": append([]string{}, m.Efforts...),
		}
		if m.Context > 0 {
			entry["context_length"] = m.Context
		}
		out[m.ID] = entry
	}
	return out
}

// hermesKeptModels keeps edits made in Hermes's model mappings. The record is
// what magpie would give each model, before user overrides: an unchanged field
// follows the catalog, while a changed or removed field remains the user's.
// A mapping from before the record existed is treated as the user's.
func hermesKeptModels(path string, want map[string]map[string]any) map[string]map[string]any {
	key := "hermes:" + path + ":catalog"
	generated, _ := json.Marshal(want)
	var wrote map[string]map[string]any
	if raw := stashShared()[key]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &wrote); err != nil {
			wrote = nil
		}
	}
	entry, _ := hermesEntry(path, magpieID).(map[string]any)
	have, _ := entry["models"].(map[string]any)
	for id, next := range want {
		prev, ok := have[id].(map[string]any)
		if !ok {
			continue
		}
		for field, value := range prev {
			old, owned := wrote[id][field]
			if !owned || !hermesMetadataEqual(value, old) {
				next[field] = value
			}
		}
		for field := range wrote[id] {
			if _, present := prev[field]; !present {
				delete(next, field)
			}
		}
	}
	if string(generated) != stashShared()[key] {
		stash(map[string]string{key: string(generated)})
	}
	return want
}

// YAML and JSON decode numbers and slices into different Go types. Comparing
// their JSON values avoids treating an untouched context or effort list as an edit.
func hermesMetadataEqual(a, b any) bool {
	left, le := json.Marshal(a)
	right, re := json.Marshal(b)
	return le == nil && re == nil && bytes.Equal(left, right)
}
