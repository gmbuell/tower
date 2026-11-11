package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Record is one session, or one registered subagent worktree (Sub) of a session.
type Record struct {
	ID         string    `json:"id"`
	Agent      string    `json:"agent"`
	Sub        bool      `json:"sub,omitempty"`
	Repo       string    `json:"repo"`
	Worktree   string    `json:"worktree"`
	Intent     string    `json:"intent"`
	Started    time.Time `json:"started"`
	LastPrompt time.Time `json:"lastPrompt"`
	PID        int       `json:"pid,omitempty"`
	PIDStart   uint64    `json:"pidStart,omitempty"`
	Ended      bool      `json:"ended,omitempty"`
	Told       string    `json:"told,omitempty"`
	Model      string    `json:"model,omitempty"`
	Transcript string    `json:"transcript,omitempty"`
}

func (r *Record) seen() time.Time {
	if r.LastPrompt.After(r.Started) {
		return r.LastPrompt
	}
	return r.Started
}

type Store struct{ dir string }

func openStore() *Store {
	dir := os.Getenv("TOWER_STATE_DIR")
	if dir == "" {
		dir = filepath.Join(configDir("XDG_STATE_HOME", filepath.Join(".local", "state")), "tower")
	}
	return &Store{dir}
}

func hashKey(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:12])
}

func (s *Store) path(r *Record) string {
	sub := ""
	if r.Sub {
		sub = r.Worktree
	}
	return filepath.Join(s.dir, "sessions", hashKey(r.ID, sub)+".json")
}

func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func (s *Store) save(r *Record) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(s.path(r), append(data, '\n'))
}

func (s *Store) remove(r *Record) { os.Remove(s.path(r)) }

func (s *Store) load(path string) *Record {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var r Record
	if json.Unmarshal(data, &r) != nil || r.ID == "" || !filepath.IsAbs(r.Repo) || !filepath.IsAbs(r.Worktree) {
		return nil
	}
	return &r
}

func (s *Store) top(id string) *Record {
	return s.load(s.path(&Record{ID: id}))
}

// subs returns the session's registered subagent records.
func (s *Store) subs(id string) []*Record {
	var recs []*Record
	for _, r := range s.all() {
		if r.Sub && r.ID == id {
			recs = append(recs, r)
		}
	}
	return recs
}

// all returns every readable record; unreadable files are ignored.
func (s *Store) all() []*Record {
	files, _ := filepath.Glob(filepath.Join(s.dir, "sessions", "*.json"))
	var recs []*Record
	for _, f := range files {
		if r := s.load(f); r != nil {
			recs = append(recs, r)
		}
	}
	return recs
}

func (s *Store) rememberRepo(repo string) {
	path := filepath.Join(s.dir, "repos", hashKey(repo))
	if _, err := os.Stat(path); err != nil {
		writeAtomic(path, []byte(repo))
	}
}

func (s *Store) forgetRepo(repo string) {
	os.Remove(filepath.Join(s.dir, "repos", hashKey(repo)))
}

func (s *Store) repos() []string {
	files, _ := filepath.Glob(filepath.Join(s.dir, "repos", "*"))
	var repos []string
	for _, f := range files {
		if data, err := os.ReadFile(f); err == nil && filepath.IsAbs(string(data)) {
			repos = append(repos, string(data))
		}
	}
	return repos
}

func (s *Store) logf(format string, args ...any) {
	appendLine(filepath.Join(s.dir, "tower.log"), fmt.Appendf(nil, time.Now().Format(time.RFC3339)+" "+format+"\n", args...))
}

// appendLine adds data to the end of a file with a single O_APPEND write, creating it if needed.
func appendLine(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func procFields(pid int) []string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return nil
	}
	fields := strings.Fields(string(data[bytes.LastIndexByte(data, ')')+1:]))
	if len(fields) < 20 || fields[0] == "Z" {
		return nil
	}
	return fields
}

// procStart is the process start time in clock ticks, or 0 when the process is gone.
// Comparing it guards against the PID being reused by another process.
func procStart(pid int) uint64 {
	fields := procFields(pid)
	if fields == nil {
		return 0
	}
	n, _ := strconv.ParseUint(fields[19], 10, 64)
	return n
}

func procParent(pid int) int {
	fields := procFields(pid)
	if fields == nil {
		return 0
	}
	n, _ := strconv.Atoi(fields[1])
	return n
}

func isAgentProc(pid int, agent string) bool {
	if comm, _ := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid)); strings.TrimSpace(string(comm)) == agent {
		return true
	}
	if exe, _ := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid)); filepath.Base(exe) == agent || strings.Contains(exe, "/"+agent+"/") {
		return true
	}
	cmdline, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	for i, arg := range strings.Split(string(cmdline), "\x00") {
		if i < 2 && filepath.Base(arg) == agent {
			return true
		}
	}
	return false
}

// agentPID finds the agent process this command runs under, or 0 when it cannot.
var agentPID = func(agent string) int {
	if agent == "claude" {
		if n, err := strconv.Atoi(os.Getenv("CLAUDE_PID")); err == nil && procStart(n) != 0 {
			return n
		}
	}
	pid := os.Getppid()
	for range 8 {
		if pid <= 1 {
			break
		}
		if isAgentProc(pid, agent) {
			return pid
		}
		pid = procParent(pid)
	}
	return 0
}

const noPIDTimeout = 12 * time.Hour

func alive(r *Record) bool {
	if r.Ended {
		return false
	}
	if r.PID > 0 {
		return r.PIDStart != 0 && procStart(r.PID) == r.PIDStart
	}
	// shortcut: no process found for this agent, so fall back to a quiet-period timeout.
	return time.Since(r.seen()) < noPIDTimeout
}

// liveSet marks live records: a session by its process, a subagent by its coordinator.
func liveSet(recs []*Record) map[*Record]bool {
	tops := map[string]bool{}
	for _, r := range recs {
		if !r.Sub && alive(r) {
			tops[r.ID] = true
		}
	}
	live := map[*Record]bool{}
	for _, r := range recs {
		live[r] = tops[r.ID] && !r.Ended
	}
	return live
}
