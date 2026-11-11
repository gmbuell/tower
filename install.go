package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

var hookEvents = []string{"SessionStart", "UserPromptSubmit", "SessionEnd", "SubagentStart"}

// editHooks adds (install) or removes tower's entries in an agent's hooks file,
// leaving every other entry in place. It reports whether the file changed.
// shortcut: the file is rewritten with keys in sorted order; keep an ordered encoder in mind if that annoys.
func editHooks(path, agent, bin string, install bool) (bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if !install {
			return false, nil
		}
		data = []byte("{}")
	} else if err != nil {
		return false, err
	}
	var root map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil || root == nil {
		return false, fmt.Errorf("%s is not a JSON object", path)
	}
	hooks, _ := root["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	ours := "tower hook " + agent
	for _, event := range hookEvents {
		groups, _ := hooks[event].([]any)
		var kept []any
		for _, group := range groups {
			g, _ := group.(map[string]any)
			handlers, _ := g["hooks"].([]any)
			if g == nil || handlers == nil {
				kept = append(kept, group)
				continue
			}
			var rest []any
			for _, handler := range handlers {
				h, _ := handler.(map[string]any)
				if command, _ := h["command"].(string); !strings.HasSuffix(command, ours) {
					rest = append(rest, handler)
				}
			}
			if len(rest) > 0 {
				g["hooks"] = rest
				kept = append(kept, g)
			}
		}
		if install {
			timeout := 10
			if event == "SessionEnd" {
				timeout = 3
			}
			// shortcut: the binary path is not shell-quoted, so it must not contain spaces.
			kept = append(kept, map[string]any{"hooks": []any{map[string]any{
				"type": "command", "command": bin + " hook " + agent, "timeout": timeout}}})
		}
		if len(kept) > 0 {
			hooks[event] = kept
		} else {
			delete(hooks, event)
		}
	}
	if len(hooks) > 0 {
		root["hooks"] = hooks
	} else {
		delete(root, "hooks")
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(root); err != nil {
		return false, err
	}
	var before, after any
	json.Unmarshal(data, &before)
	json.Unmarshal(buf.Bytes(), &after)
	if reflect.DeepEqual(before, after) {
		return false, nil
	}
	if len(data) > 2 {
		backup := path + ".tower-backup-" + time.Now().Format("20060102-150405")
		if err := os.WriteFile(backup, data, 0o600); err != nil {
			return false, err
		}
	}
	return true, writeAtomic(path, buf.Bytes())
}

func configDir(env, fallback string) string {
	if dir := os.Getenv(env); dir != "" {
		return dir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, fallback)
}

func cmdInstall(out io.Writer, install bool) error {
	bin, err := os.Executable()
	if err != nil {
		return err
	}
	if bin, err = filepath.EvalSymlinks(bin); err != nil {
		return err
	}
	targets := []struct{ agent, dir, file string }{
		{"claude", configDir("CLAUDE_CONFIG_DIR", ".claude"), "settings.json"},
		{"codex", configDir("CODEX_HOME", ".codex"), "hooks.json"},
	}
	for _, t := range targets {
		if _, err := os.Stat(t.dir); err != nil {
			fmt.Fprintf(out, "%s: %s not found, skipped\n", t.agent, t.dir)
			continue
		}
		path := filepath.Join(t.dir, t.file)
		changed, err := editHooks(path, t.agent, bin, install)
		if err != nil {
			return err
		}
		switch {
		case !changed:
			fmt.Fprintf(out, "%s: %s already up to date\n", t.agent, path)
		case install:
			fmt.Fprintf(out, "%s: hooks added to %s (backup written beside it)\n", t.agent, path)
		default:
			fmt.Fprintf(out, "%s: hooks removed from %s (backup written beside it)\n", t.agent, path)
		}
	}
	if install {
		fmt.Fprintln(out, "Codex runs new hooks only after you approve them: open Codex and run /hooks.")
	}
	return nil
}
