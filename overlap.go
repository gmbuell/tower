package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type RepoState struct {
	Repo      string
	Worktrees []*Worktree
	Live      []*Record          // live records in this repo, oldest first
	Idle      map[string]*Record // per worktree with changes and no live record: its newest dead record
}

// loadRepo sweeps one repo and drops records that no longer mean anything:
// a dead record is kept only as the last intent of a worktree that still has changes.
func loadRepo(ctx context.Context, st *Store, repo string, all []*Record) (*RepoState, error) {
	wts, err := sweep(ctx, repo)
	if err != nil {
		return nil, err
	}
	rs := &RepoState{Repo: repo, Worktrees: wts, Idle: map[string]*Record{}}
	live := liveSet(all)
	hasLive := map[string]bool{}
	for _, r := range all {
		if r.Repo == repo && live[r] {
			rs.Live = append(rs.Live, r)
			hasLive[r.Worktree] = true
		}
	}
	for _, r := range all {
		if r.Repo != repo || live[r] {
			continue
		}
		w := findWorktree(rs.Worktrees, r.Worktree)
		if w == nil || (!w.Failed && len(w.Changed) == 0) || hasLive[r.Worktree] {
			st.remove(r)
			continue
		}
		if old := rs.Idle[r.Worktree]; old == nil || r.seen().After(old.seen()) {
			if old != nil {
				st.remove(old)
			}
			rs.Idle[r.Worktree] = r
		} else {
			st.remove(r)
		}
	}
	sort.Slice(rs.Live, func(i, j int) bool { return rs.Live[i].Started.Before(rs.Live[j].Started) })
	return rs, nil
}

func findWorktree(wts []*Worktree, path string) *Worktree {
	for _, w := range wts {
		if w.Path == path {
			return w
		}
	}
	return nil
}

func (rs *RepoState) failed() bool {
	for _, w := range rs.Worktrees {
		if w.Failed {
			return true
		}
	}
	return false
}

func (rs *RepoState) liveIn(path string) []*Record {
	var recs []*Record
	for _, r := range rs.Live {
		if r.Worktree == path {
			recs = append(recs, r)
		}
	}
	return recs
}

// label says who is behind a worktree: a live session, a remembered intent, or nobody.
func (rs *RepoState) label(path string) string {
	if live := rs.liveIn(path); len(live) > 0 {
		text := fmt.Sprintf("%s session: %q", live[0].Agent, live[0].Intent)
		if len(live) > 1 {
			text += fmt.Sprintf(" and %d more", len(live)-1)
		}
		return text
	}
	if intent := rs.lastIntent(path); intent != "" {
		return fmt.Sprintf("idle, last: %q", intent)
	}
	return "idle"
}

// lastIntent is the remembered intent of a worktree nobody is working in now.
func (rs *RepoState) lastIntent(path string) string {
	if r := rs.Idle[path]; r != nil {
		return r.Intent
	}
	return ""
}

// overlapKey identifies what overlaps, apart from how it is described, so a label change is not a new warning.
func overlapKey(ovs []Overlap, sameTree []*Record) string {
	var parts []string
	for _, o := range ovs {
		parts = append(parts, o.Mine.Path, o.Other.Path, strings.Join(o.Files, "\x00"))
	}
	for _, r := range sameTree {
		parts = append(parts, "same", r.ID)
	}
	return hashKey(parts...)
}

func mainDir(repo string) string {
	if filepath.Base(repo) == ".git" {
		return filepath.Dir(repo)
	}
	return repo
}

func tilde(path string) string {
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(path, home+"/") {
		return "~" + path[len(home):]
	}
	return path
}

func wtName(repo, path string) string {
	root := mainDir(repo)
	if path == root {
		return filepath.Base(root)
	}
	if rel, err := filepath.Rel(root, path); err == nil && filepath.IsLocal(rel) {
		return rel
	}
	return tilde(path)
}

func intersect(a, b []string) []string {
	var both []string
	for i, j := 0, 0; i < len(a) && j < len(b); {
		switch {
		case a[i] == b[j]:
			both = append(both, a[i])
			i++
			j++
		case a[i] < b[j]:
			i++
		default:
			j++
		}
	}
	return both
}

type Overlap struct {
	Mine, Other *Worktree
	Files       []string
}

// overlapsFor compares a session's family (its own worktree, home, and its registered
// subagents' worktrees) with every other worktree. Nothing inside the family is compared.
// sameTree lists unrelated live sessions sharing the home working tree.
func (rs *RepoState) overlapsFor(id, home string) (ovs []Overlap, sameTree []*Record) {
	family := map[string]bool{home: true}
	for _, r := range rs.Live {
		if id != "" && r.ID == id {
			family[r.Worktree] = true
		}
	}
	for _, mine := range rs.Worktrees {
		if !family[mine.Path] {
			continue
		}
		for _, other := range rs.Worktrees {
			if family[other.Path] {
				continue
			}
			if files := intersect(mine.Changed, other.Changed); len(files) > 0 {
				ovs = append(ovs, Overlap{mine, other, files})
			}
		}
	}
	for _, r := range rs.liveIn(home) {
		if r.ID != id {
			sameTree = append(sameTree, r)
		}
	}
	return ovs, sameTree
}

func fileList(files []string, limit int) string {
	if limit > 0 && len(files) > limit {
		return fmt.Sprintf("%s, +%d more", strings.Join(files[:limit], ", "), len(files)-limit)
	}
	return strings.Join(files, ", ")
}

// describe renders overlaps as text; limit caps the files listed per worktree (0 for all).
func (rs *RepoState) describe(home string, ovs []Overlap, sameTree []*Record, limit int) string {
	var b strings.Builder
	for _, o := range ovs {
		mine := "this worktree"
		if o.Mine.Path != home {
			mine = "your subagent worktree " + wtName(rs.Repo, o.Mine.Path)
		}
		fmt.Fprintf(&b, "- %s [%s] (%s) changes the same files as %s: %s\n",
			wtName(rs.Repo, o.Other.Path), o.Other.Branch, rs.label(o.Other.Path), mine, fileList(o.Files, limit))
	}
	for _, r := range sameTree {
		fmt.Fprintf(&b, "- another live %s session is working in this same working tree: %q\n", r.Agent, r.Intent)
	}
	return b.String()
}
