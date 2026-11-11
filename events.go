package main

import (
	"encoding/json"
	"path/filepath"
	"time"
)

const eventFiles = 50

// Event is one line of the append-only history in events/<year>-<month>.jsonl.
// Session records only describe the present; this is what later analysis reads.
type Event struct {
	Time       time.Time      `json:"time"`
	Kind       string         `json:"kind"` // start, intent, warn, clear, end
	ID         string         `json:"id"`
	Agent      string         `json:"agent"`
	Sub        bool           `json:"sub,omitempty"`
	Repo       string         `json:"repo"`
	Worktree   string         `json:"worktree"`
	Intent     string         `json:"intent,omitempty"`
	Previous   string         `json:"previousIntent,omitempty"`
	Reason     string         `json:"reason,omitempty"` // start source, end reason, or how the intent was set
	Model      string         `json:"model,omitempty"`
	Transcript string         `json:"transcript,omitempty"`
	Branch     string         `json:"branch,omitempty"`
	Head       string         `json:"head,omitempty"`
	Changed    *int           `json:"changedFiles,omitempty"`
	Overlaps   []EventOverlap `json:"overlaps,omitempty"`
	SameTree   int            `json:"sameTreeSessions,omitempty"`
}

type EventOverlap struct {
	Worktree    string   `json:"worktree"`
	Other       string   `json:"other"`
	OtherLive   bool     `json:"otherLive"`
	OtherIntent string   `json:"otherIntent,omitempty"`
	FileCount   int      `json:"fileCount"`
	Files       []string `json:"files"` // at most eventFiles of them
}

func newEvent(kind string, r *Record) *Event {
	return &Event{Time: time.Now(), Kind: kind, ID: r.ID, Agent: r.Agent, Sub: r.Sub, Repo: r.Repo,
		Worktree: r.Worktree, Intent: r.Intent, Model: r.Model, Transcript: r.Transcript}
}

// at adds what git knows about the event's worktree, when a sweep is available.
func (e *Event) at(wts []*Worktree) *Event {
	if w := findWorktree(wts, e.Worktree); w != nil {
		e.Branch, e.Head = w.Branch, w.Head
		if !w.Failed {
			n := len(w.Changed)
			e.Changed = &n
		}
	}
	return e
}

func (e *Event) overlaps(rs *RepoState, ovs []Overlap, sameTree []*Record) *Event {
	for _, o := range ovs {
		row := EventOverlap{Worktree: o.Mine.Path, Other: o.Other.Path, FileCount: len(o.Files), Files: o.Files}
		if len(row.Files) > eventFiles {
			row.Files = row.Files[:eventFiles]
		}
		if live := rs.liveIn(o.Other.Path); len(live) > 0 {
			row.OtherLive, row.OtherIntent = true, live[0].Intent
		} else {
			row.OtherIntent = rs.lastIntent(o.Other.Path)
		}
		e.Overlaps = append(e.Overlaps, row)
	}
	e.SameTree = len(sameTree)
	return e
}

// record appends the event as one line. On a local filesystem one O_APPEND write per event
// keeps concurrent writers from interleaving, whatever its size; NFS gives no such guarantee.
func (s *Store) record(e *Event) {
	line, err := json.Marshal(e)
	if err == nil {
		err = appendLine(filepath.Join(s.dir, "events", e.Time.Format("2006-01")+".jsonl"), append(line, '\n'))
	}
	if err != nil {
		s.logf("event log: %v", err)
	}
}
