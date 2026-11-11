// tower shows what agent sessions are doing across local git repos and warns,
// without blocking, when two worktrees of one repo change the same files.
package main

import (
	"fmt"
	"os"
	"slices"
	"strings"
)

const usage = `tower: a board of local agent sessions, with overlap warnings between worktrees

  tower [board] [--json]   every repo with live sessions or unmerged worktrees
  tower check              overlaps for the worktree you are in
  tower intent "<text>"    set this session's intent, or register a subagent's worktree
  tower install            add tower's hooks to Claude Code and Codex
  tower uninstall          remove them
  tower hook <agent>       called by the agent hooks
`

func main() {
	args := os.Args[1:]
	command := "board"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command, args = args[0], args[1:]
	}
	st := openStore()
	cwd, _ := os.Getwd()
	var err error
	switch command {
	case "board":
		err = cmdBoard(st, os.Stdout, slices.Contains(args, "--json"))
	case "check":
		err = cmdCheck(st, os.Stdout, cwd)
	case "intent":
		err = cmdIntent(st, os.Stdout, cwd, strings.Join(args, " "))
	case "hook":
		if len(args) > 0 {
			runHook(st, args[0], os.Stdin, os.Stdout)
		}
	case "install", "uninstall":
		err = cmdInstall(os.Stdout, command == "install")
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "tower:", err)
		os.Exit(1)
	}
}
