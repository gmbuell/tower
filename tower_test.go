package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var ctx = context.Background()

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
}

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// setup makes a repo with a.txt and b.txt committed and two worktrees, wt1 and wt2.
func setup(t *testing.T) (st *Store, root, wt1, wt2 string) {
	t.Helper()
	for key, value := range map[string]string{
		"GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_SYSTEM": "/dev/null",
		"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@example.com",
		"GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@example.com",
		"CLAUDE_CODE_SESSION_ID": "", "CODEX_THREAD_ID": "",
	} {
		t.Setenv(key, value)
	}
	agentPID = func(string) int { return os.Getpid() }
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, wt1, wt2 = filepath.Join(base, "repo"), filepath.Join(base, "wt1"), filepath.Join(base, "wt2")
	os.Mkdir(root, 0o755)
	run(t, root, "git", "init", "-q", "-b", "main")
	write(t, filepath.Join(root, "a.txt"), "a\n")
	write(t, filepath.Join(root, "b.txt"), "b\n")
	run(t, root, "git", "add", ".")
	run(t, root, "git", "commit", "-q", "-m", "init")
	run(t, root, "git", "worktree", "add", "-q", wt1, "-b", "one")
	run(t, root, "git", "worktree", "add", "-q", wt2, "-b", "two")
	return &Store{filepath.Join(base, "state")}, root, wt1, wt2
}

func hook(st *Store, event, id, cwd, prompt string) string {
	input, _ := json.Marshal(map[string]string{"hook_event_name": event, "session_id": id, "cwd": cwd, "prompt": prompt})
	var out bytes.Buffer
	runHook(st, "claude", bytes.NewReader(input), &out)
	return out.String()
}

func state(t *testing.T, st *Store, root string) *RepoState {
	t.Helper()
	rs, err := loadRepo(ctx, st, filepath.Join(root, ".git"), st.all())
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

func TestOverlapBetweenWorktrees(t *testing.T) {
	st, root, wt1, wt2 := setup(t)
	write(t, filepath.Join(wt1, "a.txt"), "one\n")
	run(t, wt1, "git", "commit", "-q", "-am", "committed change")
	write(t, filepath.Join(wt1, "new.txt"), "one\n")
	write(t, filepath.Join(wt2, "a.txt"), "two\n")
	write(t, filepath.Join(wt2, "new.txt"), "two\n")
	write(t, filepath.Join(wt2, "only.txt"), "two\n")
	ovs, _ := state(t, st, root).overlapsFor("", wt1)
	if len(ovs) != 1 || ovs[0].Other.Path != wt2 || strings.Join(ovs[0].Files, ",") != "a.txt,new.txt" {
		t.Fatalf("want wt2 overlapping on a.txt and new.txt, got %+v", ovs)
	}
	if ovs, _ := state(t, st, root).overlapsFor("", root); len(ovs) != 0 {
		t.Fatalf("clean main checkout should not overlap, got %+v", ovs)
	}
}

func TestSymlinkedPathIsSameRepo(t *testing.T) {
	_, root, _, _ := setup(t)
	link := root + "-link"
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	repo, wt, err := where(ctx, link)
	if err != nil || repo != filepath.Join(root, ".git") || wt != root {
		t.Fatalf("got %q %q %v", repo, wt, err)
	}
}

func TestDeadSessions(t *testing.T) {
	st, root, wt1, wt2 := setup(t)
	gone := exec.Command("true")
	gone.Run()
	dead := gone.Process.Pid
	repo := filepath.Join(root, ".git")
	write(t, filepath.Join(wt2, "a.txt"), "two\n")
	st.save(&Record{ID: "clean", Agent: "claude", Repo: repo, Worktree: wt1, Intent: "nothing left", PID: dead, PIDStart: 1})
	st.save(&Record{ID: "dirty", Agent: "claude", Repo: repo, Worktree: wt2, Intent: "half done", PID: dead, PIDStart: 1})
	rs := state(t, st, root)
	if st.top("clean") != nil {
		t.Fatal("dead session in a clean worktree should be dropped")
	}
	if st.top("dirty") == nil || rs.label(wt2) != `idle, last: "half done"` {
		t.Fatalf("dead session with unmerged changes should stay as idle, got %q", rs.label(wt2))
	}
}

func TestSubagentFamilyAndWarnings(t *testing.T) {
	st, root, wt1, wt2 := setup(t)
	if out := hook(st, "SessionStart", "S", root, ""); !strings.Contains(out, "tower intent") {
		t.Fatalf("session start should name the commands, got %q", out)
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", "S")
	var out bytes.Buffer
	if err := cmdIntent(st, &out, wt1, "build the parser"); err != nil || !strings.Contains(out.String(), "registered") {
		t.Fatalf("subagent registration failed: %v %q", err, out.String())
	}
	write(t, filepath.Join(root, "a.txt"), "coordinator\n")
	write(t, filepath.Join(wt1, "a.txt"), "subagent\n")
	write(t, filepath.Join(wt1, "b.txt"), "subagent\n")
	if out := hook(st, "UserPromptSubmit", "S", root, "<task-notification>\nrefactor the lexer\nmore detail"); out != "" {
		t.Fatalf("no warning expected inside a family, got %q", out)
	}
	if got := st.top("S").Intent; got != "refactor the lexer" {
		t.Fatalf("intent should come from the first prompt line, got %q", got)
	}
	write(t, filepath.Join(wt2, "b.txt"), "outsider\n")
	out1 := hook(st, "UserPromptSubmit", "S", root, "next")
	if !strings.Contains(out1, "wt2") || !strings.Contains(out1, "b.txt") || !strings.Contains(out1, "subagent worktree") {
		t.Fatalf("coordinator should hear about its subagent's overlap with an outsider, got %q", out1)
	}
	if out := hook(st, "UserPromptSubmit", "S", root, "again"); out != "" {
		t.Fatalf("unchanged overlaps should not warn twice, got %q", out)
	}
	hook(st, "SessionStart", "U", wt2, "")
	if out := hook(st, "UserPromptSubmit", "S", root, "and again"); out != "" {
		t.Fatalf("a new label on the same overlap should not warn again, got %q", out)
	}
	if out := hook(st, "SessionStart", "T", root, ""); !strings.Contains(out, "same working tree") {
		t.Fatalf("a second session in the same tree should be told, got %q", out)
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", "T")
	agentPID = func(string) int { return 0 }
	if err := cmdIntent(st, &out, root, "second session"); err != nil || st.top("T").PID != os.Getpid() {
		t.Fatalf("setting an intent must keep the recorded process: %v %+v", err, st.top("T"))
	}
	hook(st, "SessionEnd", "S", root, "")
	rs := state(t, st, root)
	if len(rs.liveIn(wt1)) != 0 || rs.label(wt1) != `idle, last: "build the parser"` {
		t.Fatalf("subagent should end with its coordinator and keep its intent, got %q", rs.label(wt1))
	}
	// Files are monthly and Glob sorts, so a run across a month boundary still reads in order.
	files, _ := filepath.Glob(filepath.Join(st.dir, "events", "*.jsonl"))
	var data []byte
	for _, f := range files {
		b, _ := os.ReadFile(f)
		data = append(data, b...)
	}
	kinds := ""
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var e Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("bad event line %q: %v", line, err)
		}
		kinds += e.Kind + " "
		if e.Kind == "end" && e.Sub && e.Worktree != wt1 {
			t.Fatalf("subagent end event for the wrong worktree: %s", line)
		}
		if e.Kind == "intent" && e.Reason == "command" && (e.Branch == "" || e.Head == "" || e.Changed == nil) {
			t.Fatalf("intent event lacks git state: %s", line)
		}
		if e.Kind == "warn" && (len(e.Overlaps) != 1 || e.Overlaps[0].Other != wt2 || e.Overlaps[0].Files[0] != "b.txt") {
			t.Fatalf("warn event lacks the overlap: %s", line)
		}
	}
	if kinds != "start intent intent warn start start intent end end " {
		t.Fatalf("unexpected event sequence: %s", kinds)
	}
	var board bytes.Buffer
	if err := cmdBoard(st, &board, false); err != nil || !strings.Contains(board.String(), "build the parser") {
		t.Fatalf("board: %v\n%s", err, board.String())
	}
}

func TestHookNeverFails(t *testing.T) {
	st, _, _, _ := setup(t)
	var out bytes.Buffer
	runHook(st, "claude", strings.NewReader("not json"), &out)
	runHook(st, "claude", strings.NewReader(`{"hook_event_name":"UserPromptSubmit","session_id":"x","cwd":"/nonexistent"}`), &out)
	if out.Len() != 0 {
		t.Fatalf("expected silence, got %q", out.String())
	}
}

func TestInstallPreservesOtherHooks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	write(t, path, `{"model":"x","hooks":{"SessionStart":[{"matcher":"^startup$","hooks":[{"type":"command","command":"bash herdr.sh session","timeout":10}]}]}}`)
	for range 2 {
		if _, err := editHooks(path, "claude", "/bin/tower", true); err != nil {
			t.Fatal(err)
		}
	}
	data, _ := os.ReadFile(path)
	text := string(data)
	if strings.Count(text, "/bin/tower hook claude") != len(hookEvents) || !strings.Contains(text, "herdr.sh") || !strings.Contains(text, `"model"`) {
		t.Fatalf("install result wrong:\n%s", text)
	}
	if changed, _ := editHooks(path, "claude", "/bin/tower", true); changed {
		t.Fatal("a repeated install should change nothing")
	}
	if _, err := editHooks(path, "claude", "/bin/tower", false); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	text = string(data)
	if strings.Contains(text, "tower") || !strings.Contains(text, "herdr.sh") || !strings.Contains(text, "^startup$") {
		t.Fatalf("uninstall result wrong:\n%s", text)
	}
}
