package orchapi

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/sengirunners/potluck/kit/api"
)

// taskSandboxSize is a pool box size: c<cpus>m<GB of memory>, such as c2m8.
var taskSandboxSize = regexp.MustCompile(`^c[1-9][0-9]{0,2}m[1-9][0-9]{0,3}$`)

// egressHost is one egress allowlist entry: a host name of two or more
// labels (letters, digits, inner hyphens), optionally "*." for its
// subdomains. The driver's own rule (egressEntry in the farm's box egress
// script) is the same shape and adds its policy (no wildcard over a
// shared suffix such as *.amazonaws.com).
var egressHost = regexp.MustCompile(`^(\*\.)?[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)

// maxEgress bounds a sandbox task's extra egress hosts.
const maxEgress = 32

// numericLabel is a last label that makes a name an IPv4 address to
// inet_aton and the URL standard: decimal (127.1) or hex (0x7f.0x1).
var numericLabel = regexp.MustCompile(`^([0-9]+|0x[0-9a-f]*)$`)

// checkEgressHost refuses an egress allowlist entry that is not a host name
// or *.name: an IP address in any spelling (a numeric last label), a bare *,
// a port, a path, a scheme, whitespace or a line break.
func checkEgressHost(h string) error {
	if len(h) > 253 {
		return fmt.Errorf("an egress host is longer than 253 bytes")
	}
	l := strings.ToLower(h)
	last := l[strings.LastIndexByte(l, '.')+1:]
	if !egressHost.MatchString(l) || numericLabel.MatchString(last) {
		return fmt.Errorf("egress host %q is not a host name", h)
	}
	return nil
}

// CheckTaskSandbox refuses a sandbox request no machine could run: one that
// is also a script run or a continuation, names a size that is not a pool
// box size, or an egress entry that is not a host name. A request without
// sandbox passes.
func CheckTaskSandbox(req TaskStartRequest) error {
	sb := req.Sandbox
	switch {
	case sb == nil:
		return nil
	case req.Agent == TaskAgentRun || req.Run != nil:
		return api.Errorf(api.CodeBadRequest, "a sandbox task runs an agent in a pool box: it is not a script run")
	case req.ContinueFrom != "":
		return api.Errorf(api.CodeBadRequest, "a sandbox task does not continue another task: leave continue_from out")
	case sb.Size != "" && !taskSandboxSize.MatchString(sb.Size):
		return api.Errorf(api.CodeBadRequest, "sandbox size %q is not a pool box size", sb.Size).
			WithHint("c<cpus>m<GB>, such as c2m8 or c4m16; leave it out for the driver's default")
	case len(sb.Egress) > maxEgress:
		return api.Errorf(api.CodeBadRequest, "sandbox egress names %d hosts; at most %d", len(sb.Egress), maxEgress)
	}
	for _, h := range sb.Egress {
		if err := checkEgressHost(h); err != nil {
			return api.Errorf(api.CodeBadRequest, "sandbox %s", err.Error()).
				WithHint("name.tld or *.name.tld, such as pkg.example.com; no IP addresses, ports or bare *")
		}
	}
	return nil
}
