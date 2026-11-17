package session

// What a person has typed and not sent (SPEC 9, "Nudge"). The agent's state
// cannot show it: idle is exactly when someone is writing at the prompt. So
// the keys themselves are read: any key since the last Enter may have left text
// in the program's input, and a nudge's Enter would send it. The reading errs
// on the side of a draft (ctrl-U clears a prompt and still counts): the cost of
// a wrong "draft" is a wake that waits for the next turn, the cost of a wrong
// "no draft" is a person's half-written message sent to an agent.

import (
	"context"
	"encoding/binary"
	"hash/crc32"
	"os"
	"path/filepath"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/mezze/internal/session/holder"
	"github.com/sengirunners/potluck/kit/core"
)

// noteKeys records keys on their way to the program. Answering a hook's
// permission prompt leaves nothing behind.
func (p *proc) noteKeys(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	prev := p.lastKey
	p.lastKey = b[len(b)-1]
	if p.hookHuman {
		return nil
	}
	p.draft, p.draftKnown = !submits(prev, b), true
	if err := p.saveDraftLocked(); err != nil {
		p.unknownDraftLocked()
		return err
	}
	return nil
}

// Invalidate a durable empty proof BEFORE any human bytes reach the PTY.
// A failed or partial write must never leave empty proof for the next daemon.
func (p *proc) prepareInput() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.hookHuman {
		return nil
	}
	p.draft, p.draftKnown = true, false
	return p.saveDraftLocked()
}

func (p *proc) unknownDraftLocked() {
	p.draft, p.draftKnown, p.lastKey = true, false, '\\'
}

// submits reports whether b ends in a plain Enter: not ctrl-J, not alt-Enter
// and not backslash-Enter (prev is the key before b), all a new line in the
// same prompt. Shift-Enter and any other encoded key are not a \r either.
func submits(prev byte, b []byte) bool {
	n := len(b)
	if b[n-1] != '\r' {
		return false
	}
	if n > 1 {
		prev = b[n-2]
	}
	return prev != '\\' && prev != 0x1b
}

// The versioned record stores only knowledge and parser context, never text.
// Atomic replacement avoids exposing a partially updated empty-state proof.
func (p *proc) saveDraftLocked() error {
	if p.draftFile == "" { // isolated parser tests have no surviving holder
		return nil
	}
	state := byte('?')
	if p.draftKnown {
		state = 'e'
		if p.draft {
			state = 'd'
		}
	}
	f, err := os.CreateTemp(filepath.Dir(p.draftFile), ".draft-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	record := make([]byte, 9)
	copy(record, []byte{'z', 'd', 1, state, p.lastKey})
	binary.LittleEndian.PutUint32(record[5:], crc32.ChecksumIEEE(record[:5]))
	if _, err = f.Write(record); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(f.Name(), p.draftFile); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(p.draftFile))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// mayNudge: nobody can be typing in the session and nothing typed is waiting
// in it. A client that can type is a person's terminal, so the mail waits for
// their next turn's notice. A read-only viewer does not count.
func (p *proc) mayNudge() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state == orchapi.StateIdle && len(p.conns) == 0 && p.draftKnown && !p.draft
}

// typeNudge is typeLine that looks again once it holds the writes: a key from
// before that is in draft or conns, and a key from after waits until the Enter
// has gone, so it can neither be sent nor land inside the line.
func (p *proc) typeNudge(line string) error {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	if !p.mayNudge() {
		return nil
	}
	return p.typeLocked(line, true)
}

// wakeAfterDetach gives an agent the wake it was refused while a person had its
// terminal open: when the last of them leaves, nothing is left in the prompt
// and mail addressed to the session is unread. Group mail already moved on to
// a session nobody had open, and announcements and notices wake nobody.
func (s *Service) wakeAfterDetach(ctx context.Context, p *proc) {
	if s.ctx.Err() != nil || p.isExited() || !p.mayNudge() || !s.unreadToSession(ctx, p.addr) {
		return
	}
	if _, err := s.Nudge(ctx, p.addr, NudgeLine); err != nil {
		s.d.Log.Debug("wake after detach", "session", p.addr, "err", err)
	}
}

// unreadToSession reports unread mail that names address itself, here and, on
// a replica, as of its last look at the authority. Local reads only.
func (s *Service) unreadToSession(ctx context.Context, address string) bool {
	m, ok := orchcore.Mail(s.d).(orchcore.TeamMail)
	if !ok {
		return false
	}
	parts, err := m.NoticeParts(ctx, core.SubjectOf(ctx).ForSession(address))
	if err != nil {
		s.d.Log.Debug("wake after detach", "session", address, "err", err)
		return false
	}
	if tr, ok := orchcore.Team(s.d).(orchcore.TeamReplica); ok {
		parts = append(parts, tr.TeamNotice(address)...)
	}
	for _, n := range parts {
		if n.DirectWakeable != nil {
			if *n.DirectWakeable > 0 {
				return true
			}
			continue
		}
		// Old peers do not report overlap between quiet and group counts.
		// Under-count conservatively until their next upgraded presence answer.
		if n.Unread-n.Group-n.Announce-n.Owners-n.Quiet > 0 {
			return true
		}
	}
	return false
}

// loadDraft is what a daemon adopting a session knows of it from before.
func (s *Service) loadDraft(p *proc) {
	p.draftFile = holder.DraftPath(s.d.Config.Home, p.id8)
	p.unknownDraftLocked()
	b, err := os.ReadFile(p.draftFile)
	if err != nil {
		return
	}
	if len(b) == 1 { // old daemon's nonempty marker retains its parser byte
		p.draftKnown, p.lastKey = true, b[0]
		return
	}
	if len(b) != 9 || b[0] != 'z' || b[1] != 'd' || b[2] != 1 || binary.LittleEndian.Uint32(b[5:]) != crc32.ChecksumIEEE(b[:5]) {
		return
	}
	switch b[3] {
	case 'd':
		p.draftKnown, p.lastKey = true, b[4]
	case 'e':
		if b[4] == 0 || b[4] == '\r' {
			p.draft, p.draftKnown, p.lastKey = false, true, b[4]
		}
	}
}

// Only creation of a new child proves no person's input preceded tracking.
func (s *Service) initDraft(p *proc) error {
	p.draftFile = holder.DraftPath(s.d.Config.Home, p.id8)
	p.draft, p.draftKnown, p.lastKey = false, true, 0
	return p.saveDraftLocked()
}
