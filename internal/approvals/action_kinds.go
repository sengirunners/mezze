package approvals

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"math/big"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/api/textclean"
	"github.com/sengirunners/potluck/kit/authz"
)

// The action kinds that run something as the approver (SPEC 16 "Asks that
// carry an action"): run, command and authorize-key start on a named
// machine of the project through the team module (orchcore.AskActions);
// browser is
// refused until a runtime enforces its scope. Each is checked and frozen
// here when the ask is made: what the approver reads is exactly what runs.

// runsSomething reports whether kind starts work as the approver (only a
// signed answer may pick it).
func runsSomething(kind string) bool {
	return kind == orchapi.ActionRun || kind == orchapi.ActionCommand || kind == orchapi.ActionBrowser || kind == orchapi.ActionAuthorizeKey
}

// signedOnly reports whether picking an action of kind takes a signed
// answer (a paired device or the Mezze app's key, never the owner's socket, which
// every process of their OS user reaches): what runs something, and a hand
// brief, which preapproves the approver's own agent.
func signedOnly(kind string) bool { return runsSomething(kind) || kind == orchapi.ActionHand }

// cleanHand checks a hand brief and freezes it: an agent and its brief, no
// machine (it runs on the approver's own, in the Mezze app).
func cleanHand(sub *authz.Subject, project, area string, in orchapi.AskAction, on string) (*orchapi.AskAction, error) {
	bad := func(format string, a ...any) error {
		return api.Errorf(api.CodeBadRequest, format, a...).
			WithHint("zm ask new --hand --agent claude --brief <file> --verify '<check>' --ack-on <choice>")
	}
	if !sub.Can(authz.Operate, authz.Object{Project: project, Area: area}) {
		return nil, api.Errorf(api.CodeForbidden, "an action ask is made by who may operate on its area")
	}
	if in.Machine != "" || in.Script != "" || in.Command != "" || in.Ref != "" || in.Commit != "" || len(in.Args) > 0 ||
		in.Plan != "" || in.Scope != nil || in.User != "" || in.Key != "" || len(in.IDs) > 0 {
		return nil, bad("a hand brief takes only --agent and --brief: it runs on the approver's own machine")
	}
	if in.Agent != "claude" && in.Agent != "codex" {
		return nil, bad("a hand brief's agent is claude or codex, not %q", in.Agent)
	}
	brief := strings.TrimSpace(in.Brief)
	if brief == "" || len(brief) > orchapi.MaxActionBrief || !utf8.ValidString(brief) || strings.ContainsRune(brief, 0) {
		return nil, bad("a hand brief is 1 to %d bytes of text", orchapi.MaxActionBrief)
	}
	return &orchapi.AskAction{Kind: orchapi.ActionHand, On: on, Agent: in.Agent, Brief: brief, BriefSHA: textSHA(brief)}, nil
}

// askActions is the team module's side (the lane host a test set), or nil.
func (s *Service) askActions() orchcore.AskActions {
	if s.lanes != nil {
		return s.lanes
	}
	t, _ := orchcore.Team(s.d).(orchcore.AskActions)
	return t
}

var (
	userRe   = regexp.MustCompile(`^[a-z_][a-z0-9_.-]{0,31}$`)
	plainArg = regexp.MustCompile(`^[A-Za-z0-9_./:=@%+,-]+$`)
)

const (
	maxActionArgs  = 32
	maxActionField = 400 // a script path, ref, argument or machine name
)

// cleanRunning checks an action that runs something and freezes it. The
// asker may operate on the ask's area; the machine is a trusted machine of
// the project that takes remote tasks.
func (s *Service) cleanRunning(ctx context.Context, sub *authz.Subject, project, area string, in orchapi.AskAction, on string) (*orchapi.AskAction, error) {
	bad := func(format string, a ...any) error {
		return api.Errorf(api.CodeBadRequest, format, a...).
			WithHint("zm ask new --help: --run-on, --browser-on or --authorize-key, and --ack-on for the choice that runs it")
	}
	if !sub.Can(authz.Operate, authz.Object{Project: project, Area: area}) {
		return nil, api.Errorf(api.CodeForbidden, "an action ask is made by who may operate on its area")
	}
	if len(in.IDs) > 0 || in.Count != 0 || len(in.Samples) > 0 {
		return nil, bad("ids are for an %s action", orchapi.ActionAckMail)
	}
	in.Machine = strings.TrimSpace(in.Machine)
	for _, f := range append([]string{in.Machine, in.Script, in.Ref, in.Commit, in.User, in.Agent}, in.Args...) {
		if len(f) > maxActionField || !utf8.ValidString(f) || strings.ContainsAny(f, "\x00\r\n") {
			return nil, bad("action fields are single lines of at most %d bytes", maxActionField)
		}
	}
	if in.Machine == "" {
		return nil, bad("a %s action names its machine", in.Kind)
	}
	aa := s.askActions()
	if aa == nil {
		return nil, api.Errorf(api.CodeUnavailable, "this daemon runs no actions on machines")
	}
	out := orchapi.AskAction{Kind: in.Kind, On: on, Machine: in.Machine}
	switch in.Kind {
	case orchapi.ActionRun:
		if pa, err := api.ParseAddress(sub.Address); err != nil || pa.Kind != api.KindSession {
			return nil, bad("a run action is asked by a session: its result mails it")
		}
		if in.Plan != "" || in.Scope != nil || in.User != "" || in.Key != "" {
			return nil, bad("a run action takes a script or an agent brief, nothing else")
		}
		script := in.Script != "" || in.Ref != "" || in.Commit != "" || len(in.Args) > 0
		switch {
		case script && (in.Agent != "" && in.Agent != orchapi.TaskAgentRun || in.Brief != ""):
			return nil, bad("a run action is a script run or an agent task, not both")
		case script:
			in.Commit = strings.ToLower(strings.TrimSpace(in.Commit))
			if in.Script == "" || in.Ref == "" || !orchapi.RunCommitOK(in.Commit) {
				return nil, bad("a script run needs script, ref and commit: the full 40-character commit id ref points at now (git rev-parse <ref>)")
			}
			if why := orchapi.RunRefProblem(in.Ref); why != "" {
				return nil, bad("%s", why)
			}
			if len(in.Args) > maxActionArgs {
				return nil, bad("a script run takes at most %d arguments", maxActionArgs)
			}
			out.Script, out.Ref, out.Commit, out.Args = in.Script, in.Ref, in.Commit, in.Args
			out.Command = commandLine(in.Script, in.Args)
		case in.Agent == "claude" || in.Agent == "codex":
			if strings.TrimSpace(in.Brief) == "" || len(in.Brief) > orchapi.MaxActionBrief || !utf8.ValidString(in.Brief) {
				return nil, bad("an agent task's brief is 1 to %d bytes", orchapi.MaxActionBrief)
			}
			out.Agent, out.Brief, out.BriefSHA = in.Agent, in.Brief, textSHA(in.Brief)
		default:
			return nil, bad("a run action needs a script (script, ref, commit) or an agent (claude or codex) and a brief")
		}
		if _, err := aa.AskActionMachine(ctx, project, in.Machine, script); err != nil {
			return nil, err
		}
		if script {
			var err error
			if out.PinChecked, err = aa.AskActionPin(ctx, project, out.Ref, out.Commit); err != nil {
				return nil, err
			}
		}
	case orchapi.ActionCommand:
		if pa, err := api.ParseAddress(sub.Address); err != nil || pa.Kind != api.KindSession {
			return nil, bad("a command action is asked by a session: its result mails it")
		}
		if in.Script != "" || len(in.Args) > 0 || in.Agent != "" || in.Brief != "" || in.Plan != "" || in.Scope != nil || in.User != "" || in.Key != "" {
			return nil, bad("a command action takes a machine, a command, a ref and a commit, nothing else")
		}
		if err := checkCommand(in.Command); err != nil {
			return nil, bad("command: %v", err)
		}
		in.Commit = strings.ToLower(strings.TrimSpace(in.Commit))
		if in.Ref == "" || !orchapi.RunCommitOK(in.Commit) {
			return nil, bad("a command runs in a clean checkout of ref at commit: give the ref and the full 40-character commit id it points at now (git rev-parse <ref>)")
		}
		if why := orchapi.RunRefProblem(in.Ref); why != "" {
			return nil, bad("%s", why)
		}
		st, err := aa.AskActionMachine(ctx, project, in.Machine, true)
		if err != nil {
			return nil, err
		}
		if !st.CommandRuns {
			return nil, api.Errorf(api.CodeUnavailable, "%s's zm runs no command actions", in.Machine).
				WithHint("update zm there (zm daemon update), or commit the step as a script and use --script")
		}
		out.Command, out.Ref, out.Commit = in.Command, in.Ref, in.Commit
		if out.PinChecked, err = aa.AskActionPin(ctx, project, out.Ref, out.Commit); err != nil {
			return nil, err
		}
	case orchapi.ActionAuthorizeKey:
		if in.Script != "" || in.Ref != "" || in.Commit != "" || len(in.Args) > 0 || in.Agent != "" || in.Brief != "" || in.Plan != "" || in.Scope != nil {
			return nil, bad("an authorize-key action takes a machine, a user and a key, nothing else")
		}
		if !userRe.MatchString(in.User) {
			return nil, bad("user %q: the account's name on %s", in.User, in.Machine)
		}
		key, comment, fp, err := parseAuthorizedKey(in.Key)
		if err != nil {
			return nil, bad("key: %v", err)
		}
		st, err := aa.AskActionMachine(ctx, project, in.Machine, false)
		if err != nil {
			return nil, err
		}
		if st.OSUser == "" {
			return nil, api.Errorf(api.CodeUnavailable, "%s's zm does not say which user it runs as", in.Machine).WithHint("update zm there (zm daemon update)")
		}
		if st.OSUser != in.User {
			return nil, api.Errorf(api.CodeBadRequest, "zm runs as %s on %s, not %s", st.OSUser, in.Machine, in.User).
				WithHint("ask that machine's owner to run zm as that user, or pick its user (zm never uses sudo)")
		}
		out.User, out.Key, out.KeyComment, out.KeyFingerprint = in.User, key, comment, fp
	case orchapi.ActionBrowser:
		if strings.TrimSpace(in.Plan) == "" || len(in.Plan) > orchapi.MaxActionPlan || in.Scope == nil || len(in.Scope.Origins) == 0 {
			return nil, bad("a browser action needs a plan (at most %d bytes) and a scope with its origins", orchapi.MaxActionPlan)
		}
		for _, o := range in.Scope.Origins {
			if u, err := url.Parse(o); err != nil || u.Scheme != "https" || u.Host == "" {
				return nil, bad("origin %q: https://host", o)
			}
		}
		return nil, api.Errorf(api.CodeUnavailable, "browser actions do not run yet: no browser runtime here enforces their scope").
			WithHint("ask for a judgement instead, and do the browser step yourself or file it as work")
	}
	mp, err := s.machinePerson(ctx, project, in.Machine)
	if err != nil {
		return nil, err
	}
	out.MachinePerson = mp
	return &out, nil
}

// commandLine is a script run as a person reads it, quoted for a shell.
func commandLine(script string, args []string) string {
	parts := []string{shellQuote(script)}
	for _, a := range args {
		parts = append(parts, shellQuote(a))
	}
	return strings.Join(parts, " ")
}

func shellQuote(s string) string {
	if plainArg.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Key types an authorize-key action takes (rsa only at 3072 bits or more).
var keyCurves = map[string]string{"ecdsa-sha2-nistp256": "nistp256", "ecdsa-sha2-nistp384": "nistp384", "ecdsa-sha2-nistp521": "nistp521"}

// parseAuthorizedKey checks one OpenSSH public key line (no options) and
// returns it normalised ("type base64 comment"), its comment and its
// SHA256: fingerprint.
func parseAuthorizedKey(line string) (key, comment, fp string, err error) {
	line = strings.TrimSpace(line)
	if line == "" || strings.ContainsAny(line, "\r\n\x00") || len(line) > 16<<10 {
		return "", "", "", fmt.Errorf("one OpenSSH public key line (the .pub file's contents)")
	}
	f := strings.Fields(line)
	if len(f) < 2 {
		return "", "", "", fmt.Errorf("one OpenSSH public key line: type, key and a comment")
	}
	typ := f[0]
	blob, err := base64.StdEncoding.DecodeString(f[1])
	if err != nil {
		return "", "", "", fmt.Errorf("the key is not base64")
	}
	r := wire(blob)
	if got, ok := r.str(); !ok || string(got) != typ {
		return "", "", "", fmt.Errorf("the key's type does not match %s", typ)
	}
	switch typ {
	case "ssh-ed25519":
		if k, ok := r.str(); !ok || len(k) != 32 {
			return "", "", "", fmt.Errorf("not an ed25519 key")
		}
	case "ecdsa-sha2-nistp256", "ecdsa-sha2-nistp384", "ecdsa-sha2-nistp521":
		curve, ok := r.str()
		q, ok2 := r.str()
		if !ok || !ok2 || string(curve) != keyCurves[typ] || len(q) < 65 || q[0] != 4 {
			return "", "", "", fmt.Errorf("not an %s key", typ)
		}
	case "ssh-rsa":
		_, ok := r.str() // e
		n, ok2 := r.str()
		if !ok || !ok2 {
			return "", "", "", fmt.Errorf("not an rsa key")
		}
		if bits := new(big.Int).SetBytes(n).BitLen(); bits < 3072 {
			return "", "", "", fmt.Errorf("an rsa key of %d bits: at least 3072 (or use ed25519)", bits)
		}
	default:
		return "", "", "", fmt.Errorf("key type %q: ssh-ed25519, ecdsa-sha2-nistp256/384/521 or ssh-rsa (an options prefix is not taken)", typ)
	}
	if len(r) != 0 {
		return "", "", "", fmt.Errorf("the key has trailing bytes")
	}
	comment = strings.TrimSpace(textclean.Clean(strings.Join(f[2:], " ")))
	if len(comment) > 200 || unsafePresentation(comment) {
		return "", "", "", fmt.Errorf("the key's comment is at most 200 plain characters")
	}
	sum := sha256.Sum256(blob)
	key = typ + " " + f[1]
	if comment != "" {
		key += " " + comment
	}
	return key, comment, "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:]), nil
}

// wire reads SSH wire format strings.
type wire []byte

func (w *wire) str() ([]byte, bool) {
	if len(*w) < 4 {
		return nil, false
	}
	n := binary.BigEndian.Uint32(*w)
	if uint64(n) > uint64(len(*w)-4) {
		return nil, false
	}
	out := (*w)[4 : 4+n]
	*w = (*w)[4+n:]
	return out, true
}

// runningText is a running kind's action as the approver reads it: exactly
// what runs, where and as whom.
func runningText(act *orchapi.AskAction) string {
	on := strings.ToUpper(act.On)
	switch act.Kind {
	case orchapi.ActionRun:
		if act.Brief != "" {
			return fmt.Sprintf("If you choose %s, an agent (%s) on %s will work from this brief, as you: %s (brief sha256 %s). It decides the exact commands itself. Any other answer does nothing.",
				on, act.Agent, act.Machine, quoted(briefHead(act.Brief)), short(act.BriefSHA))
		}
		return fmt.Sprintf("If you choose %s, zm runs this on %s, as you: %s, from %s at commit %s%s.%s Any other answer does nothing.",
			on, act.Machine, act.Command, act.Ref, act.Commit, pinNote(act), riskNote(act.Command))
	case orchapi.ActionHand:
		return fmt.Sprintf("If you choose %s, your own agent (%s) on your own machine works from this brief, as you, with full access: this answer approves it (in the Mezze app: Hand to my agent). %s (brief sha256 %s). Any other answer does nothing.",
			on, act.Agent, quoted(briefHead(act.Brief)), short(act.BriefSHA))
	case orchapi.ActionCommand:
		return fmt.Sprintf("If you choose %s, zm runs this command with /bin/sh on %s, as you, in a clean checkout of %s at commit %s%s: %s%s Its output comes back to the asker and the work item. Any other answer does nothing.",
			on, act.Machine, act.Ref, act.Commit, pinNote(act), act.Command, riskNote(act.Command))
	case orchapi.ActionAuthorizeKey:
		from := ""
		if act.KeyComment != "" {
			from = ", from " + quoted(act.KeyComment)
		}
		return fmt.Sprintf("If you choose %s, zm adds this key to the %s account on %s: %s%s. Any other answer does nothing.",
			on, act.User, act.Machine, act.KeyFingerprint, from)
	case orchapi.ActionBrowser:
		sign, submit := "signs in nowhere", "never presses a final button (send, pay, delete)"
		if act.Scope != nil && act.Scope.SignIn != "" {
			sign = "signs in as " + quoted(act.Scope.SignIn)
		}
		if act.Scope != nil && act.Scope.Submit {
			submit = "may press final buttons (send, pay, delete)"
		}
		origins := ""
		if act.Scope != nil {
			origins = strings.Join(act.Scope.Origins, ", ")
		}
		return fmt.Sprintf("If you choose %s, a browser on %s follows this plan: %s. It opens only %s, %s and %s. Any other answer does nothing.",
			on, act.Machine, quoted(act.Plan), origins, sign, submit)
	}
	return ""
}

// pinNote says, after a run's commit, that it was the ref's head when the
// ask was made (zm checked it then): it runs at exactly that commit even
// if the ref has moved on by the time of the yes.
func pinNote(act *orchapi.AskAction) string {
	if !act.PinChecked {
		return ""
	}
	return ", the head of " + act.Ref + " when this was asked"
}

// briefHead is a brief's first lines, at most 300 bytes.
func briefHead(b string) string {
	lines := strings.Split(strings.TrimSpace(b), "\n")
	if len(lines) > 3 {
		lines = append(lines[:3], "…")
	}
	out := strings.Join(lines, " / ")
	if len(out) > 300 {
		out = strings.ToValidUTF8(out[:300], "") + "…"
	}
	return out
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
