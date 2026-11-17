package main

// A committed swap and signals: from its first change
// a swap does not stop for SIGINT/SIGTERM; it ends within its budget and
// answers each signal with when it ends and what it waits on.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sengirunners/potluck/kit/swap"
)

// stepTo names what the swap does now: a signal past commit prints it.
func (s *swapper) stepTo(format string, args ...any) {
	step := fmt.Sprintf(format, args...)
	s.mu.Lock()
	s.step, s.since = step, time.Now()
	s.mu.Unlock()
	if onStep != nil {
		onStep(step)
	}
}

// onStep runs as each step begins (tests: a kill at a given step).
var onStep func(step string)

// swapSignals are the signals a committed swap answers instead of obeying
// (main's signal context catches them too).
var swapSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}

// beforeCommit runs at the start of commit (tests: a signal just before it).
var beforeCommit func()

// commit makes the rest of the swap deaf to SIGINT/SIGTERM. Called before
// the first change (a placed file, a stop): from there a cancelled context
// would leave the child it started unverified, the files restored under
// it and a report of "the daemon is down". From here the swap is
// bounded by swapFinishBudget instead (every call and wait in it has its
// own deadline too), so it still ends: verified, or rolled back (or
// forward), and reported as it is. A signal before commit, even just
// before it, stops the swap with nothing changed.
func (s *swapper) commit() error {
	if s.unhook != nil {
		return nil
	}
	if beforeCommit != nil {
		beforeCommit()
	}
	sig := s.ctx
	// Listen before the check: a signal from here on is seen by the watch
	// or stops the swap here, never neither.
	ch := make(chan os.Signal, 4)
	signal.Notify(ch, swapSignals...)
	if err := sig.Err(); err != nil {
		signal.Stop(ch)
		return fmt.Errorf("interrupted before the swap changed anything: %w", err)
	}
	// A swapper killed from here (SIGKILL, a stop timeout, OOM) can leave its
	// build running unverified: the marker tells the next update tick.
	// Only an upgrade under swap.lock writes one.
	if s.o.what == "upgrade" && s.lock != nil {
		p := swap.Unfinished{What: s.o.what, From: s.rec.From, To: s.pf.Build, PID: os.Getpid(), At: time.Now().UTC()}
		if err := swap.WritePending(s.cfg.Home, p); err != nil {
			signal.Stop(ch)
			return fmt.Errorf("cannot record the swap before it changes anything: %w", err)
		}
		s.pending = true
	}
	s.base, s.end = context.WithoutCancel(sig), time.Now().Add(swapFinishBudget)
	s.until(s.end.Add(-swapRollbackReserve))
	s.stepTo("starting")
	quit, ended := make(chan struct{}), make(chan struct{})
	go s.watch(sig, ch, quit, ended)
	s.unhook = func() {
		signal.Stop(ch)
		close(quit)
		// The watch may be stuck writing to a stalled stderr (a full pipe
		// nobody drains): a finished swap returns anyway.
		select {
		case <-ended:
		case <-time.After(watchExitWait):
		}
	}
	return nil
}

// sameSignal is how long after main's signal context ends the watch waits
// for the signal that ended it (the two hear one signal, in either order).
const sameSignal = 100 * time.Millisecond

// watchExitWait bounds how long a finished swap waits for its signal watch
// to exit.
var watchExitWait = 2 * time.Second

// watch answers signals past commit. The swap does not stop:
// stopping mid-swap is what left an unverified daemon on restored files,
// and there is no hard escape (DESIGN-NOTES, "Safe swaps"). The
// first signal says when it ends; each later one prints its step and the
// time left. sig ending without a signal (a caller's cancel) counts as
// the first.
func (s *swapper) watch(sig context.Context, ch <-chan os.Signal, quit, ended chan struct{}) {
	defer close(ended)
	told := false
	first := func() {
		told = true
		fmt.Fprintf(os.Stderr, "zm: finishing the swap: it ends within %s; Ctrl-C again to see its progress\n", s.left())
	}
	done := sig.Done()
	for {
		select {
		case <-quit:
			return
		case <-done:
			done = nil
			if told {
				continue
			}
			select {
			case <-ch:
			case <-time.After(sameSignal):
			case <-quit:
				return
			}
			first()
		case <-ch:
			if !told {
				first()
				continue
			}
			s.mu.Lock()
			step, since := s.step, s.since
			s.mu.Unlock()
			fmt.Fprintf(os.Stderr, "zm: the swap is %s (%s so far); it ends within %s, verified, rolled back or rolled forward\n",
				step, time.Since(since).Round(time.Second), s.left())
		}
	}
}

// left is the committed swap's time left, to the second.
func (s *swapper) left() time.Duration {
	return max(time.Until(s.end), 0).Round(time.Second)
}

// until bounds the rest of the swap by t (within its budget).
func (s *swapper) until(t time.Time) {
	ctx, cancel := context.WithDeadline(s.base, t)
	s.ctx, s.cancels = ctx, append(s.cancels, cancel)
}

// onRollback runs at the start of a rollback (tests: lengthen its waits).
var onRollback func()

// rollingBack gives a committed swap's rollback the rest of the budget.
func (s *swapper) rollingBack() {
	if s.rolling {
		return
	}
	s.rolling = true
	if onRollback != nil {
		onRollback()
	}
	if s.base != nil {
		s.until(s.end)
	}
}

// outOfTime is err, said as the budget when that is what ended it.
func (s *swapper) outOfTime(err error) error {
	if errors.Is(err, context.DeadlineExceeded) && s.base != nil {
		return fmt.Errorf("the swap ran out of its %s budget", swapFinishBudget)
	}
	return err
}

// done removes the pending marker once the swap ended verified, rolled
// back or forward, or as it began (untouched), then stops commit's signal
// watch (main's signal context stays until it exits) and drops its
// deadlines. Any other end (a failure that may leave the new build
// running) keeps the marker for the next update tick. The marker goes
// first: the watch's bounded wait never sits between a finished swap and
// its cleanup.
func (s *swapper) done() {
	if s.pending {
		ended := s.untouched
		switch s.rec.Result {
		case "ok", "rolled_back", "rolled_forward":
			ended = true
		}
		if ended {
			if err := swap.ClearPending(s.cfg.Home); err != nil {
				fmt.Fprintln(os.Stderr, "zm: could not remove", swap.FilePending+":", err)
			}
		}
	}
	if s.unhook != nil {
		s.unhook()
	}
	for _, cancel := range s.cancels {
		cancel()
	}
}
