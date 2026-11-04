package agent

import (
	"encoding/json"
	"slices"
	"strings"
)

// hermesModels gives Hermes explicit per-model metadata without changing its
// callable IDs. group is the public upstream prefix, not a models.dev maker.
// An empty reasoning_efforts list explicitly offers no thinking choices.
func hermesModels() map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, m := range sharedModels("hermes") {
		group, _, _ := strings.Cut(m.ID, "/")
		levels := []string{}
		for _, level := range m.Efforts {
			if level == "off" {
				level = "none"
			}
			if !slices.Contains(levels, level) {
				levels = append(levels, level)
			}
		}
		entry := map[string]any{
			"name": m.Name, "group": group,
			"reasoning_efforts": levels,
		}
		if m.Context > 0 {
			entry["context_length"] = m.Context
		}
		out[m.ID] = entry
	}
	return out
}

// hermesKeptModels prepares the merged mapping and its pending ownership
// record. Changed and deleted fields remain user-owned through future catalog
// equality or absence. This builder never writes the record.
func hermesKeptModels(path string, want map[string]map[string]any) (map[string]map[string]any, string) {
	key := "hermes:" + path + ":catalog"
	previous, known := catalogOwnershipIn(stashShared()[key])
	entry, _ := hermesEntry(path, magpieID).(map[string]any)
	have, _ := entry["models"].(map[string]any)
	current := map[string]map[string]any{}
	for id, value := range have {
		if model, ok := value.(map[string]any); ok {
			current[id] = model
		}
	}
	merged, record := keepCatalogMetadata(current, want, previous, known,
		"name", "group", "context_length", "reasoning_efforts")
	data, _ := json.Marshal(record)
	return merged, string(data)
}
