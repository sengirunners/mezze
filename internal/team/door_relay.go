package team

// The Mezze app door's deliveries (SPEC 18.4): the door module at a team
// project's authority hands each delivery to the machine whose Mezze app owns the
// task (orchcore.DoorRelay). To the authority's own machine it posts at once;
// to another it queues a door_delivery machine op (teamlink's queue), taken by
// the machine's held-open poll or its next heartbeat answer, recorded
// before it runs and run at most once there, its report posted at once
// (ReportNow) or carried by the next beat. The body and headers never reach
// the queue's table: the authority holds them in memory for the one
// call (teamlink.OpMem), filled in for the answer that carries the op
// (doorArgs).

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"syscall"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/teamlink"
)

var _ orchcore.DoorRelay = (*Service)(nil)

// doorPostTimeout bounds the machine's POST to the Mezze app.
var doorPostTimeout = 30 * time.Second

// doorStale: a machine whose last heartbeat here is older than this is not
// waited on (presence goes stale at 60 s too).
const doorStale = 60 * time.Second

// doorClient posts only to loopback (the dialer refuses anything else,
// whatever localhost resolves to) and never follows a redirect.
var doorClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	Transport: &http.Transport{
		Proxy: nil,
		DialContext: (&net.Dialer{Timeout: 5 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
				return &net.AddrError{Err: "the door posts only to loopback", Addr: address}
			}
			return nil
		}}).DialContext,
		DisableKeepAlives: true,
	},
}

// RelayDoor implements orchcore.DoorRelay.
func (s *Service) RelayDoor(ctx context.Context, project, machine string, op orchapi.DoorDeliveryOp, wait time.Duration) (orchapi.DoorDeliveryResult, error) {
	if machine == s.d.Config.Machine {
		return postDoor(ctx, op)
	}
	if !s.d.Store.IsTeam(project) {
		return orchapi.DoorDeliveryResult{}, api.NotFound("machine", machine)
	}
	if time.Since(s.LastBeat(project, machine)) > doorStale {
		return orchapi.DoorDeliveryResult{}, api.Errorf(api.CodeUnavailable, "%s has not been heard from in %s", machine, doorStale)
	}
	s.ExpireOps(ctx)
	payload, _ := json.Marshal(orchapi.DoorDeliveryOp{Task: op.Task, Target: op.Target}) // without the body and headers: they stay out of the database
	mop := api.MachineOp{ID: ids.New(), Kind: opDoorDelivery, Payload: payload}
	args, _ := json.Marshal(orchapi.DoorDeliveryOp{Headers: op.Headers, Body: op.Body})
	e := s.doorMem.Put(mop.ID, args)
	defer s.doorMem.Forget(mop.ID)
	if err := s.QueueOp(ctx, teamlink.Op{MachineOp: mop, Project: project, Machine: machine, FromMachine: s.d.Config.Machine}); err != nil {
		return orchapi.DoorDeliveryResult{}, err
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-e.Done():
		r := e.Result()
		if err := r.Err(); err != nil {
			return orchapi.DoorDeliveryResult{}, err
		}
		var res orchapi.DoorDeliveryResult
		if json.Unmarshal(r.Raw, &res) != nil || res.Status < 100 || res.Status > 999 {
			return orchapi.DoorDeliveryResult{}, api.Errorf(api.CodeUnavailable, "%s answered no status", machine)
		}
		return res, nil
	case <-t.C:
	case <-ctx.Done():
	}
	// No answer: an op no answer carried yet never runs; one carried may
	// still, once (the Mezze app's replay dedupe drops a second copy).
	_, _ = s.UpdateOp(context.WithoutCancel(ctx), mop.ID, teamlink.OpUpdate{State: teamlink.Str(teamlink.OpDone), Error: teamlink.Str("not taken in time"), Unsent: true})
	return orchapi.DoorDeliveryResult{}, api.Errorf(api.CodeUnavailable, "%s did not answer within %s", machine, wait)
}

// opDoorDelivery hands a Mezze app door delivery to the machine whose Mezze app owns
// the task (SPEC 18.4): payload orchapi.DoorDeliveryOp (its body and headers
// filled in only for the answer that carries it), report
// orchapi.DoorDeliveryResult, posted at once.
const opDoorDelivery = "door_delivery"

// registerDoorOps registers door_delivery with teamlink's queue.
func (s *Service) registerDoorOps() {
	s.RegisterOp(opDoorDelivery, teamlink.OpHandler{Run: s.runDoorOp, Apply: s.applyDoorReport, Args: s.doorArgs,
		Expiry: relayOpTake, ReportNow: true})
}

// doorArgs fills in a door_delivery op's body and headers for each answer
// that carries it; an error keeps it out. It answers no waiting call.
func (s *Service) doorArgs(_ context.Context, _ string, op *api.MachineOp) error {
	args, ok := s.doorMem.Args(op.ID)
	var d orchapi.DoorDeliveryOp
	if !ok || json.Unmarshal(op.Payload, &d) != nil {
		return api.Errorf(api.CodeUnavailable, "not taken in time")
	}
	var full orchapi.DoorDeliveryOp
	if json.Unmarshal(args, &full) != nil {
		return api.Errorf(api.CodeUnavailable, "not taken in time")
	}
	d.Headers, d.Body = full.Headers, full.Body
	op.Payload, _ = json.Marshal(d)
	return nil
}

// applyDoorReport takes the machine's report on a door_delivery op.
func (s *Service) applyDoorReport(ctx context.Context, o teamlink.Op, rep api.MachineOpReport) {
	r := teamlink.OpResult{Code: rep.Code, Msg: rep.Error, Hint: rep.Hint, Fail: rep.Error != ""}
	if !r.Fail {
		if len(rep.Payload) == 0 {
			r = teamlink.OpResult{Fail: true, Msg: "the machine answered no result"}
		} else {
			r.Raw = rep.Payload
		}
	}
	s.doorMem.Finish(o.ID, r)
	_, _ = s.UpdateOp(ctx, o.ID, teamlink.OpUpdate{State: teamlink.Str(teamlink.OpDone)})
}

// ---- the machine ----

// runDoorOp posts a relayed delivery to this machine's Mezze app.
func (s *Service) runDoorOp(ctx context.Context, _ string, op api.MachineOp) (json.RawMessage, error) {
	var d orchapi.DoorDeliveryOp
	if len(op.Payload) == 0 || json.Unmarshal(op.Payload, &d) != nil {
		return nil, api.Errorf(api.CodeBadRequest, "no delivery")
	}
	res, err := postDoor(ctx, d)
	if err != nil {
		e, ok := err.(*api.Error)
		if !ok {
			e = api.Errorf(api.CodeUnavailable, "%v", err)
		}
		return nil, e
	}
	b, _ := json.Marshal(res)
	return b, nil
}

// postDoor posts a delivery to its target on this machine (checked again
// here: loopback, the Mezze app task's hook path) and answers the Mezze app's status.
func postDoor(ctx context.Context, op orchapi.DoorDeliveryOp) (orchapi.DoorDeliveryResult, error) {
	if err := orchapi.CheckDoorTarget(op.Target); err != nil {
		return orchapi.DoorDeliveryResult{}, err
	}
	cctx, cancel := context.WithTimeout(ctx, doorPostTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, op.Target, bytes.NewReader(op.Body))
	if err != nil {
		return orchapi.DoorDeliveryResult{}, api.Errorf(api.CodeBadRequest, "target: %v", err)
	}
	for k, vs := range op.Headers {
		if orchapi.DoorDropHeader(k) {
			continue
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := doorClient.Do(req)
	if err != nil {
		return orchapi.DoorDeliveryResult{}, api.Errorf(api.CodeUnavailable, "the Mezze app did not answer at the task's hook URL: %v", err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	return orchapi.DoorDeliveryResult{Status: resp.StatusCode}, nil
}
