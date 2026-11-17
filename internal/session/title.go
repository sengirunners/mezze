package session

// Terminal titles (SPEC 8): a managed session's program sets its terminal's
// title with OSC 0 or OSC 2 (ESC ] 0 ; text BEL, or ST = ESC \ instead of
// BEL); Claude Code sets the conversation's topic. The read loop feeds every
// PTY chunk through a titleParser (sequences split across reads included;
// no copy of the output is kept, only the title being read) and the latest
// title is stored as term_title at most once per titleEvery.

import (
	"bytes"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
)

// titleEvery spaces term_title writes (tests shorten it).
var titleEvery = time.Second

// maxOSC bounds the title bytes kept while a sequence is read; the rest of
// a longer one is dropped (the stored title is clipped far below this).
const maxOSC = 2048

// Parser states.
const (
	tsNormal  = iota
	tsEsc     // saw ESC
	tsNum     // saw ESC ]: reading the OSC number
	tsText    // reading an OSC 0/2 title
	tsTextEsc // saw ESC inside a title (ESC \ ends it)
	tsSkip    // inside another OSC: skipped to its end
	tsSkipEsc // saw ESC inside a skipped OSC
)

const (
	escByte    = 0x1b
	belByte    = 0x07
	stByte     = '\\' // ESC \ is ST
	oscIntro   = ']'
	oscSepByte = ';'
	maxOSCNum  = 1 << 16
)

// titleParser finds OSC 0/2 titles in a byte stream. Not safe for
// concurrent use (only the session's read loop feeds it).
type titleParser struct {
	st     int
	num    int
	digits int
	buf    []byte
}

// feed reads one chunk and reports the last title completed in it.
func (t *titleParser) feed(b []byte) (title string, ok bool) {
	if t.st == tsNormal && bytes.IndexByte(b, escByte) < 0 {
		return "", false
	}
	for i := 0; i < len(b); i++ {
		c := b[i]
		switch t.st {
		case tsNormal:
			j := bytes.IndexByte(b[i:], escByte)
			if j < 0 {
				return title, ok
			}
			i += j
			t.st = tsEsc
		case tsEsc:
			switch c {
			case oscIntro:
				t.st, t.num, t.digits = tsNum, 0, 0
			case escByte:
			default:
				t.st = tsNormal
			}
		case tsNum:
			switch {
			case c >= '0' && c <= '9' && t.num < maxOSCNum:
				t.num, t.digits = t.num*10+int(c-'0'), t.digits+1
			case c == oscSepByte && t.digits > 0 && (t.num == 0 || t.num == 2):
				t.st, t.buf = tsText, t.buf[:0]
			case c == belByte:
				t.st = tsNormal
			case c == escByte:
				t.st = tsSkipEsc
			default:
				t.st = tsSkip
			}
		case tsText:
			switch c {
			case belByte:
				title, ok, t.st = clipLine(string(t.buf)), true, tsNormal
			case escByte:
				t.st = tsTextEsc
			default:
				if len(t.buf) < maxOSC {
					t.buf = append(t.buf, c)
				}
			}
		case tsTextEsc:
			if c == stByte {
				title, ok, t.st = clipLine(string(t.buf)), true, tsNormal
				continue
			}
			t.st = tsEsc // an unterminated title: drop it, read this ESC's sequence
			i--
		case tsSkip:
			switch c {
			case belByte:
				t.st = tsNormal
			case escByte:
				t.st = tsSkipEsc
			}
		case tsSkipEsc:
			if c == stByte {
				t.st = tsNormal
				continue
			}
			t.st = tsEsc
			i--
		}
	}
	return title, ok
}

// titleSeen records the latest title the read loop parsed and schedules
// its write, at most one per titleEvery.
func (s *Service) titleSeen(p *proc, title string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if title == p.title {
		return
	}
	p.title = title
	if p.titleTimer != nil {
		return // the scheduled write takes the latest title
	}
	p.titleTimer = time.AfterFunc(max(0, titleEvery-time.Since(p.titleAt)), func() { s.flushTitle(p) })
}

// flushTitle stores the latest title as term_title (an updated event when
// it changed).
func (s *Service) flushTitle(p *proc) {
	p.mu.Lock()
	title := p.title
	p.titleTimer, p.titleAt = nil, time.Now()
	p.mu.Unlock()
	if _, err := s.update(s.ctx, p.id8, orchapi.EventSessionUpdated, func(r *row) (bool, error) {
		if r.State == orchapi.StateEnded || r.TermTitle == title {
			return false, nil
		}
		r.TermTitle = title
		return true, nil
	}); err != nil && s.ctx.Err() == nil {
		s.d.Log.Debug("session term title", "session", p.id8, "err", err)
	}
}

// flushTitleNow writes a pending title at once (the session is ending).
func (s *Service) flushTitleNow(p *proc) {
	p.mu.Lock()
	pending := p.titleTimer != nil && p.titleTimer.Stop()
	p.mu.Unlock()
	if pending {
		s.flushTitle(p)
	}
}
