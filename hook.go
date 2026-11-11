package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode"
)

const (
	hookBudget  = 2 * time.Second
	intentRunes = 100
	warnFiles   = 5
)

type hookInput struct {
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
	Event     string `json:"hook_event_name"`
	Prompt    string `json:"prompt"`
	AgentID   string `json:"agent_id"`
	Model     string `json:"model"`
	Path      string `json:"transcript_path"`
	Source    string `json:"source"`
	Reason    string `json:"reason"`
}

// note copies what only the hooks know onto the record, so later events can carry it.
func (in hookInput) note(r *Record) {
	if in.Model != "" {
		r.Model = clean(in.Model)
	}
	if in.Path != "" && len(in.Path) <= 4096 {
		r.Transcript = in.Path
	}
}

// clean makes a one-line, printable, bounded intent out of arbitrary text.
func clean(text string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.Join(strings.Fields(strings.Map(func(r rune) rune {
			if unicode.IsControl(r) || !unicode.IsPrint(r) {
				return ' '
			}
			return r
		}, line)), " ")
		if line == "" {
			continue
		}
		if runes := []rune(line); len(runes) > intentRunes {
			line = string(runes[:intentRunes-1]) + "…"
		}
		return line
	}
	return ""
}

// promptIntent is the first prompt line that is ordinary text, not a harness tag.
func promptIntent(prompt string) string {
	for _, line := range strings.Split(prompt, "\n") {
		if text := clean(line); text != "" && !strings.HasPrefix(text, "<") {
			return text
		}
	}
	return ""
}

// runHook handles one agent hook call. It never fails the agent: every problem is logged and swallowed.
func runHook(st *Store, agent string, in io.Reader, out io.Writer) {
	defer func() {
		if p := recover(); p != nil {
			st.logf("hook panic: %v", p)
		}
	}()
	if agent != "claude" && agent != "codex" {
		return
	}
	var input hookInput
	data, _ := io.ReadAll(io.LimitReader(in, 1<<20))
	if json.Unmarshal(data, &input) != nil || input.SessionID == "" || len(input.SessionID) > 200 || input.Cwd == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), hookBudget)
	defer cancel()
	text, err := "", error(nil)
	switch {
	case input.Event == "SubagentStart":
		if _, _, werr := where(ctx, input.Cwd); werr == nil {
			text = "If you are doing implementation work in your own git worktree, you may register it on the tower board by running `tower intent \"<one line describing your work>\"` from that worktree."
		}
	case input.AgentID != "":
		// Other events fired inside a subagent belong to the coordinator's record.
	case input.Event == "SessionStart":
		text, err = hookStart(ctx, st, agent, input)
	case input.Event == "UserPromptSubmit":
		text, err = hookPrompt(ctx, st, agent, input)
	case input.Event == "SessionEnd":
		if r := st.top(input.SessionID); r != nil {
			r.Ended = true
			if err = st.save(r); err == nil {
				// Subagent records end with their coordinator (see liveSet), so their ends are logged here too.
				for _, rec := range append([]*Record{r}, st.subs(r.ID)...) {
					end := newEvent("end", rec)
					end.Reason = clean(input.Reason)
					st.record(end)
				}
			}
		}
	}
	if err != nil && !errors.Is(err, errNoRepo) {
		st.logf("hook %s %s: %v", agent, input.Event, err)
	}
	if text != "" {
		json.NewEncoder(out).Encode(map[string]any{"hookSpecificOutput": map[string]any{
			"hookEventName": input.Event, "additionalContext": text}})
	}
}

// upsert creates or refreshes the session's record for wherever it is now.
func upsert(ctx context.Context, st *Store, agent, id, cwd string) (*Record, error) {
	repo, worktree, err := where(ctx, cwd)
	if err != nil {
		return nil, err
	}
	r := st.top(id)
	if r == nil {
		r = &Record{ID: id, Started: time.Now()}
	}
	r.Agent, r.Repo, r.Worktree, r.Ended = agent, repo, worktree, false
	if r.PID == 0 || procStart(r.PID) != r.PIDStart {
		r.PID = agentPID(agent)
		r.PIDStart = procStart(r.PID)
	}
	// The session is now in this worktree itself, so an earlier subagent record for it is redundant.
	st.remove(&Record{ID: id, Sub: true, Worktree: worktree})
	st.rememberRepo(repo)
	return r, nil
}

func hookStart(ctx context.Context, st *Store, agent string, input hookInput) (string, error) {
	r, err := upsert(ctx, st, agent, input.SessionID, input.Cwd)
	if err != nil {
		return "", err
	}
	input.note(r)
	if err := st.save(r); err != nil {
		return "", err
	}
	start := newEvent("start", r)
	start.Reason = clean(input.Source)
	st.record(start)
	var b strings.Builder
	b.WriteString("tower tracks which agent sessions are working in this repository and warns when two worktrees change the same files.\n" +
		"Run `tower intent \"<one line>\"` once you understand the task, `tower check` to see overlapping changes, and `tower board` for everything in flight.\n")
	all := st.all()
	live := liveSet(all)
	header := false
	for _, other := range all {
		if !live[other] || other.Repo != r.Repo || other.ID == r.ID {
			continue
		}
		if !header {
			b.WriteString("Other live sessions in this repository:\n")
			header = true
		}
		same := ""
		if other.Worktree == r.Worktree {
			same = " (in this same working tree)"
		}
		fmt.Fprintf(&b, "- %s, %s: %q%s\n", wtName(r.Repo, other.Worktree), other.Agent, other.Intent, same)
	}
	return b.String(), nil
}

func hookPrompt(ctx context.Context, st *Store, agent string, input hookInput) (string, error) {
	r, err := upsert(ctx, st, agent, input.SessionID, input.Cwd)
	if err != nil {
		return "", err
	}
	r.LastPrompt = time.Now()
	input.note(r)
	first := false
	if r.Intent == "" {
		r.Intent = promptIntent(input.Prompt)
		first = r.Intent != ""
	}
	if err := st.save(r); err != nil {
		return "", err
	}
	rs, err := loadRepo(ctx, st, r.Repo, st.all())
	if first {
		intent := newEvent("intent", r)
		intent.Reason = "prompt"
		if rs != nil {
			intent.at(rs.Worktrees)
		}
		st.record(intent)
	}
	if err != nil {
		return "", err
	}
	if ctx.Err() != nil || rs.failed() {
		return "", errors.New("overlap sweep exceeded its time budget")
	}
	ovs, sameTree := rs.overlapsFor(r.ID, r.Worktree)
	told := ""
	if len(ovs)+len(sameTree) > 0 {
		told = overlapKey(ovs, sameTree)
	}
	if told == r.Told {
		return "", nil
	}
	r.Told = told
	if err := st.save(r); err != nil {
		return "", err
	}
	if told == "" {
		st.record(newEvent("clear", r).at(rs.Worktrees))
		return "", nil
	}
	st.record(newEvent("warn", r).at(rs.Worktrees).overlaps(rs, ovs, sameTree))
	return "tower: other work in this repository overlaps with yours.\n" +
		rs.describe(r.Worktree, ovs, sameTree, warnFiles) +
		"This is advisory. Run `tower check` for the full list.", nil
}

// sessionFromEnv identifies the agent session a shell command runs in.
// When one agent launched the other, both IDs are set and the nearest agent process wins.
// Codex's sandbox hides parent processes, so when neither is visible the session is Codex's.
func sessionFromEnv() (id, agent string) {
	claude, codex := os.Getenv("CLAUDE_CODE_SESSION_ID"), os.Getenv("CODEX_THREAD_ID")
	if claude != "" && codex != "" {
		for pid := os.Getppid(); pid > 1; pid = procParent(pid) {
			if isAgentProc(pid, "codex") {
				break
			}
			if isAgentProc(pid, "claude") {
				return claude, "claude"
			}
		}
		return codex, "codex"
	}
	if claude != "" {
		return claude, "claude"
	}
	if codex != "" {
		return codex, "codex"
	}
	return "", ""
}

// cmdIntent sets the session's intent, or registers a subagent when run from another worktree.
func cmdIntent(st *Store, out io.Writer, cwd, text string) error {
	text = clean(text)
	if text == "" {
		return errors.New(`usage: tower intent "<one line describing the work>"`)
	}
	id, agent := sessionFromEnv()
	if id == "" {
		return errors.New("no Claude Code or Codex session found in the environment")
	}
	ctx, cancel := context.WithTimeout(context.Background(), hookBudget)
	defer cancel()
	repo, worktree, err := where(ctx, cwd)
	if err != nil {
		return err
	}
	top := st.top(id)
	if top == nil {
		if top, err = upsert(ctx, st, agent, id, cwd); err != nil {
			return err
		}
	}
	if top.Worktree == worktree {
		// An existing record is left alone apart from its intent: inside Codex's sandbox the
		// agent process is invisible, and refreshing the record would lose its process ID.
		previous := top.Intent
		top.Intent = text
		if err := st.save(top); err != nil {
			return err
		}
		logIntent(st, top, previous)
		fmt.Fprintln(out, "tower: intent recorded")
		return nil
	}
	sub := &Record{ID: id, Agent: agent, Sub: true, Repo: repo, Worktree: worktree, Started: time.Now(),
		Model: top.Model, Transcript: top.Transcript}
	previous := ""
	if old := st.load(st.path(sub)); old != nil {
		sub.Started, previous = old.Started, old.Intent
	}
	sub.Intent, sub.LastPrompt = text, time.Now()
	if err := st.save(sub); err != nil {
		return err
	}
	st.rememberRepo(repo)
	logIntent(st, sub, previous)
	fmt.Fprintf(out, "tower: registered %s under this session\n", wtName(repo, worktree))
	return nil
}

// logIntent records an intent set by command, with the worktree's git state when it can be read in time.
// Only the record's own worktree is read, under its own budget, so this stays cheap in repos with many worktrees.
func logIntent(st *Store, r *Record, previous string) {
	ctx, cancel := context.WithTimeout(context.Background(), hookBudget)
	defer cancel()
	e := newEvent("intent", r)
	e.Previous, e.Reason = previous, "command"
	wts, mainHead, err := listWorktrees(ctx, r.Repo)
	if err != nil {
		st.logf("intent event: %v", err)
	}
	if w := findWorktree(wts, r.Worktree); w != nil {
		if w.Changed, w.Failed = changed(ctx, w, mainHead); w.Failed {
			st.logf("intent event: could not read changes in %s", w.Path)
		}
		e.at(wts)
	}
	st.record(e)
}

// cmdCheck prints every overlap for the worktree the caller stands in.
func cmdCheck(st *Store, out io.Writer, cwd string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*hookBudget)
	defer cancel()
	repo, worktree, err := where(ctx, cwd)
	if err != nil {
		return err
	}
	rs, err := loadRepo(ctx, st, repo, st.all())
	if err != nil {
		return err
	}
	id, _ := sessionFromEnv()
	ovs, sameTree := rs.overlapsFor(id, worktree)
	if len(ovs)+len(sameTree) == 0 {
		fmt.Fprintln(out, "tower: no overlaps")
	} else {
		fmt.Fprint(out, rs.describe(worktree, ovs, sameTree, 0))
	}
	if rs.failed() {
		fmt.Fprintln(out, "tower: some worktrees could not be read, so this may be incomplete")
	}
	return nil
}
