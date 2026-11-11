package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

type boardSession struct {
	ID         string    `json:"id"`
	Agent      string    `json:"agent"`
	Subagent   bool      `json:"subagent"`
	Intent     string    `json:"intent"`
	Started    time.Time `json:"started"`
	LastPrompt time.Time `json:"lastPrompt"`
}

type boardWorktree struct {
	Name       string         `json:"name"`
	Path       string         `json:"path"`
	Branch     string         `json:"branch"`
	Changed    int            `json:"changedFiles"`
	Sessions   []boardSession `json:"sessions"`
	LastIntent string         `json:"lastIntent,omitempty"`
}

type boardOverlap struct {
	A     string   `json:"a"`
	B     string   `json:"b"`
	Files []string `json:"files"`
}

type boardRepo struct {
	Name      string          `json:"name"`
	Path      string          `json:"path"`
	Worktrees []boardWorktree `json:"worktrees"`
	Overlaps  []boardOverlap  `json:"overlaps"`
}

// buildBoard sweeps every known repo and keeps those with live sessions or unmerged worktrees.
func buildBoard(st *Store) []boardRepo {
	all := st.all()
	known := map[string]bool{}
	for _, repo := range st.repos() {
		known[repo] = true
	}
	for _, r := range all {
		known[r.Repo] = true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var mu sync.Mutex
	var wg sync.WaitGroup
	var board []boardRepo
	for repo := range known {
		if _, err := os.Stat(repo); err != nil {
			st.forgetRepo(repo)
			for _, r := range all {
				if r.Repo == repo {
					st.remove(r)
				}
			}
			continue
		}
		wg.Go(func() {
			rs, err := loadRepo(ctx, st, repo, all)
			if err != nil {
				return
			}
			if row := boardFor(rs); len(row.Worktrees) > 0 {
				mu.Lock()
				board = append(board, row)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	sort.Slice(board, func(i, j int) bool { return board[i].Path < board[j].Path })
	return board
}

func boardFor(rs *RepoState) boardRepo {
	row := boardRepo{Name: filepath.Base(mainDir(rs.Repo)), Path: mainDir(rs.Repo), Overlaps: []boardOverlap{}}
	var shown []*Worktree
	for _, w := range rs.Worktrees {
		live := rs.liveIn(w.Path)
		if len(live) == 0 && len(w.Changed) == 0 {
			continue
		}
		entry := boardWorktree{Name: wtName(rs.Repo, w.Path), Path: w.Path, Branch: w.Branch, Changed: len(w.Changed), Sessions: []boardSession{}}
		for _, r := range live {
			entry.Sessions = append(entry.Sessions, boardSession{r.ID, r.Agent, r.Sub, r.Intent, r.Started, r.LastPrompt})
		}
		if len(live) == 0 {
			entry.LastIntent = rs.lastIntent(w.Path)
		}
		row.Worktrees = append(row.Worktrees, entry)
		shown = append(shown, w)
	}
	for i, a := range shown {
		for _, b := range shown[i+1:] {
			if files := intersect(a.Changed, b.Changed); len(files) > 0 && !sameFamily(rs, a.Path, b.Path) {
				row.Overlaps = append(row.Overlaps, boardOverlap{wtName(rs.Repo, a.Path), wtName(rs.Repo, b.Path), files})
			}
		}
	}
	return row
}

func sameFamily(rs *RepoState, a, b string) bool {
	for _, x := range rs.liveIn(a) {
		for _, y := range rs.liveIn(b) {
			if x.ID == y.ID {
				return true
			}
		}
	}
	return false
}

func age(t time.Time) string {
	d := time.Since(t)
	switch {
	case t.IsZero():
		return "no prompt yet"
	case d < time.Minute:
		return "active just now"
	case d < time.Hour:
		return fmt.Sprintf("active %dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("active %dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("active %dd ago", int(d.Hours()/24))
}

func cmdBoard(st *Store, out io.Writer, asJSON bool) error {
	board := buildBoard(st)
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if board == nil {
			board = []boardRepo{}
		}
		return enc.Encode(map[string]any{"repos": board})
	}
	if len(board) == 0 {
		fmt.Fprintln(out, "tower: nothing in flight")
		return nil
	}
	sessions := 0
	var b strings.Builder
	for _, repo := range board {
		fmt.Fprintf(&b, "\n%s  %s\n", repo.Name, tilde(repo.Path))
		line := func(indent string, w boardWorktree, who string) {
			fmt.Fprintf(&b, "%s%s [%s]  %s  · %d changed\n", indent, w.Name, w.Branch, who, w.Changed)
		}
		type placed struct {
			s boardSession
			w boardWorktree
		}
		var live []placed
		tops := map[string]bool{}
		for _, w := range repo.Worktrees {
			for _, s := range w.Sessions {
				live = append(live, placed{s, w})
				if !s.Subagent {
					tops[s.ID] = true
				}
			}
		}
		slices.SortStableFunc(live, func(x, y placed) int { return x.s.Started.Compare(y.s.Started) })
		// Sessions first, each followed by its registered subagents; then idle worktrees.
		for _, p := range live {
			if p.s.Subagent && tops[p.s.ID] {
				continue
			}
			sessions++
			kind := p.s.Agent
			if p.s.Subagent {
				kind += " subagent"
			}
			line("  ", p.w, fmt.Sprintf("%s  %q  · %s", kind, p.s.Intent, age(p.s.LastPrompt)))
			if p.s.Subagent {
				continue
			}
			for _, sub := range live {
				if sub.s.Subagent && sub.s.ID == p.s.ID {
					line("    ↳ ", sub.w, fmt.Sprintf("subagent  %q", sub.s.Intent))
				}
			}
		}
		for _, w := range repo.Worktrees {
			if len(w.Sessions) > 0 {
				continue
			}
			who := "idle"
			if w.LastIntent != "" {
				who = fmt.Sprintf("idle, last: %q", w.LastIntent)
			}
			line("  ", w, who)
		}
		for _, o := range repo.Overlaps {
			fmt.Fprintf(&b, "  ! %s and %s both change: %s\n", o.A, o.B, fileList(o.Files, warnFiles))
		}
	}
	fmt.Fprintf(out, "tower · %d live sessions · %d repos\n%s", sessions, len(board), b.String())
	return nil
}
