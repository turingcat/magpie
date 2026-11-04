package agent

import (
	"bytes"
	"encoding/json"
	"maps"
)

// catalogOwnership distinguishes generated values from durable user overrides.
// A user flag also represents a deletion: the live file supplies the value, or
// its absence, even when future generated values coincide or disappear.
type catalogOwnership struct {
	Version   int                        `json:"version"`
	Generated map[string]map[string]any  `json:"generated"`
	User      map[string]map[string]bool `json:"user,omitempty"`
}

func catalogOwnershipIn(raw string) (catalogOwnership, bool) {
	var record catalogOwnership
	if raw == "" || json.Unmarshal([]byte(raw), &record) != nil {
		return catalogOwnership{}, false
	}
	if record.Version != 0 {
		return record, record.Version == 1 && record.Generated != nil
	}
	// The first exporter stored just the generated model mapping.
	var generated map[string]map[string]any
	if json.Unmarshal([]byte(raw), &generated) != nil || generated == nil {
		return catalogOwnership{}, false
	}
	return catalogOwnership{Version: 1, Generated: generated}, true
}

func keepCatalogMetadata(have, want map[string]map[string]any, previous catalogOwnership, known bool, fields ...string) (map[string]map[string]any, catalogOwnership) {
	out := make(map[string]map[string]any, len(want))
	record := catalogOwnership{Version: 1, Generated: want, User: map[string]map[string]bool{}}
	for id, next := range want {
		merged := maps.Clone(next)
		out[id] = merged
		current, exists := have[id]
		if !exists {
			continue
		}
		user := maps.Clone(previous.User[id])
		if user == nil {
			user = map[string]bool{}
		}
		if !known || previous.Generated[id] == nil {
			// An unknown record claims neither existing values nor their
			// absence. New model entries still receive generated metadata.
			for _, field := range fields {
				user[field] = true
			}
			for field := range next {
				user[field] = true
			}
		}
		for field, value := range current {
			old, generated := previous.Generated[id][field]
			if !generated || !catalogMetadataEqual(value, old) {
				user[field] = true
			}
		}
		for field := range previous.Generated[id] {
			if _, present := current[field]; !present {
				user[field] = true
			}
		}
		for field := range user {
			if value, present := current[field]; present {
				merged[field] = value
			} else {
				delete(merged, field)
			}
		}
		if len(user) > 0 {
			record.User[id] = user
		}
	}
	return out, record
}

// JSON comparison treats the equivalent number and slice types decoded from
// TOML, YAML and the ownership record alike.
func catalogMetadataEqual(a, b any) bool {
	left, le := json.Marshal(a)
	right, re := json.Marshal(b)
	return le == nil && re == nil && bytes.Equal(left, right)
}
