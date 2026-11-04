package agent

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/yetone/magpie/internal/edit"
	"gopkg.in/yaml.v3"
)

// hermesWrite holds one operation's fixed profile roster and pending records.
// All files it can change are in the existing editor's rollback boundary.
type hermesWrite struct {
	at     place
	others []string
	stash  stashChanges
}

func hermesChanging(at place, dir string, extra []string, run func(*hermesWrite) error) error {
	w := &hermesWrite{at: at, others: hermesOthers(at, dir), stash: stashChanges{}}
	configs := append([]string{filepath.Join(dir, "config.yaml")}, w.others...)
	configs = append(configs, extra...)
	var paths []string
	seen := map[string]bool{}
	for _, path := range configs {
		for _, p := range []string{path, hermesEnvFile(path)} {
			if !seen[p] {
				paths = append(paths, p)
				seen[p] = true
			}
		}
	}
	return edit.Atomically(func() error {
		if err := run(w); err != nil {
			return err
		}
		return stashUpdate(w.stash)
	}, paths...)
}

func (w *hermesWrite) provider(path string, kvs ...edit.KV) error {
	value, record, env := hermesKept(w.at, path, hermesProviderAt(w.at.gw()))
	if env != nil {
		p := hermesEnvFile(path)
		if f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); err == nil {
			if err := f.Close(); err != nil {
				return err
			}
		} else if !errors.Is(err, fs.ErrExist) {
			return err
		}
		if err := edit.SetEnvFile(p, *env); err != nil {
			return err
		}
	}
	var err error
	if len(kvs) == 0 && edit.GetYAMLMap(path, "providers."+magpieID) != nil {
		// syncYAML avoids rewriting a semantically unchanged provider.
		// Validate the read first; its legacy missing-value behavior must
		// not turn an unreadable config into a successful ownership write.
		raw, readErr := edit.Read(path)
		if readErr != nil {
			return readErr
		}
		var doc any
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		err = syncYAML(path, "providers."+magpieID, func() any { return value })
	} else {
		kvs = append([]edit.KV{{Path: "providers." + magpieID, Value: value}}, kvs...)
		err = edit.SetYAML(path, kvs...)
	}
	if err != nil {
		return err
	}
	w.stash["hermes:"+path+":catalog"] = record
	return nil
}
