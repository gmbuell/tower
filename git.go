package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	// Never take the index lock: an agent may be running git in the same worktree.
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	out, err := cmd.Output()
	return string(out), err
}

var errNoRepo = errors.New("not inside a git working tree")

// where returns the repo identity (resolved common git dir) and the resolved worktree root.
func where(ctx context.Context, cwd string) (repo, worktree string, err error) {
	out, err := git(ctx, cwd, "rev-parse", "--path-format=absolute", "--show-toplevel", "--git-common-dir")
	if err != nil {
		return "", "", errNoRepo
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		return "", "", errNoRepo
	}
	if worktree, err = filepath.EvalSymlinks(lines[0]); err != nil {
		return "", "", err
	}
	if repo, err = filepath.EvalSymlinks(lines[1]); err != nil {
		return "", "", err
	}
	return repo, worktree, nil
}

type Worktree struct {
	Path    string
	Head    string
	Branch  string
	Main    bool
	Changed []string // sorted paths relative to the worktree root
	Failed  bool     // git did not answer, so Changed is unknown
}

// sweep lists the repo's worktrees and what each one changed.
func sweep(ctx context.Context, repo string) ([]*Worktree, error) {
	wts, mainHead, err := listWorktrees(ctx, repo)
	if err != nil {
		return nil, err
	}
	var wg sync.WaitGroup
	for _, w := range wts {
		wg.Go(func() { w.Changed, w.Failed = changed(ctx, w, mainHead) })
	}
	wg.Wait()
	return wts, nil
}

// listWorktrees lists the repo's worktrees without reading their changes, plus the main checkout's HEAD.
func listWorktrees(ctx context.Context, repo string) ([]*Worktree, string, error) {
	out, err := git(ctx, repo, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, "", err
	}
	var wts []*Worktree
	mainHead := ""
	for i, block := range strings.Split(strings.TrimSpace(out), "\n\n") {
		w := &Worktree{Branch: "detached"}
		skip := false
		for _, line := range strings.Split(block, "\n") {
			key, val, _ := strings.Cut(line, " ")
			switch key {
			case "worktree":
				w.Path = val
			case "HEAD":
				w.Head = val
			case "branch":
				w.Branch = strings.TrimPrefix(val, "refs/heads/")
			case "bare", "prunable":
				skip = true
			}
		}
		if i == 0 && !skip {
			w.Main, mainHead = true, w.Head
		}
		path, err := filepath.EvalSymlinks(w.Path)
		if skip || err != nil {
			continue
		}
		w.Path = path
		wts = append(wts, w)
	}
	return wts, mainHead, nil
}

// changed is uncommitted and untracked files, plus (outside the main checkout)
// everything committed since the worktree branched from the main checkout's HEAD.
func changed(ctx context.Context, w *Worktree, mainHead string) ([]string, bool) {
	set := map[string]bool{}
	out, err := git(ctx, w.Path, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return nil, true
	}
	parts := strings.Split(out, "\x00")
	for i := 0; i < len(parts); i++ {
		entry := parts[i]
		if len(entry) < 4 {
			continue
		}
		set[entry[3:]] = true
		if strings.ContainsAny(entry[:2], "RC") {
			i++
			if i < len(parts) {
				set[parts[i]] = true
			}
		}
	}
	if !w.Main && mainHead != "" && w.Head != mainHead {
		if base, err := git(ctx, w.Path, "merge-base", "HEAD", mainHead); err == nil {
			out, err := git(ctx, w.Path, "diff", "--name-only", "-z", "--no-renames", strings.TrimSpace(base), "HEAD")
			if err != nil {
				return nil, true
			}
			for _, file := range strings.Split(out, "\x00") {
				set[file] = true
			}
		}
	}
	files := []string{}
	for file := range set {
		// A trailing slash is a nested repository (such as a worktree folder), not a file.
		if file != "" && !strings.HasSuffix(file, "/") {
			files = append(files, file)
		}
	}
	sort.Strings(files)
	return files, false
}
