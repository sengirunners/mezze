package fulltest

import (
	"context"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchclient"
)

// The key directory (SPEC 17.12): the machine owner's key published in the
// hidden project and at harbor's project level, where the guest of
// harbor/solver must not see it. Every secret-keys route answers the guest
// exactly as for a missing key, no list or error names it, and the guest
// can neither publish nor revoke.
func TestSecretKeysLeak(t *testing.T) {
	f := newLeakFixture(t)
	ctx := context.Background()
	var fp, seal string
	for _, p := range []string{"zz-side", "harbor"} {
		k, err := orchclient.PublishSecretKey(ctx, f.ownerAt(p, ""))
		if err != nil {
			t.Fatal(err)
		}
		fp, seal = k.Fingerprint, k.SealKey
	}
	f.forbid(fp, "a hidden seal key's fingerprint")
	f.forbid(seal, "a hidden seal key")
	f.hidden = append(f.hidden, fp)

	only := func(p string) bool { return !strings.Contains(p, "/v0/secret-keys") }
	for _, tr := range transports {
		f.walk(t, tr.String(), f.dm.Router.Routes(), func(method, target, project string) leakResp {
			return f.call(method, target, project, tr)
		}, only, nil)
	}
	for _, q := range []string{"", "?all=1", "?to=machine:mini", "?to=human:ivo"} {
		if r := f.call("GET", "/v0/secret-keys"+q, "", viaTailnet); r.status != 200 || strings.TrimSpace(r.body) != `{"keys":[]}` {
			t.Fatalf("guest list %s: %d %s", q, r.status, r.body)
		}
	}
	if r := f.call("PUT", "/v0/secret-keys", "harbor/solver", viaTailnet); r.status != 403 {
		t.Fatalf("the guest published a key: %d %s", r.status, r.body)
	}
	if r := f.call("POST", "/v0/secret-keys/"+fp+"/revoke", "harbor/solver", viaTailnet); r.status != 404 {
		t.Fatalf("the guest's revoke of a hidden key: %d %s", r.status, r.body)
	}
	if l, err := orchclient.SecretKeys(ctx, f.ownerAt("", ""), "", false); err != nil || len(l) != 2 {
		t.Fatalf("the owner sees %d keys: %v", len(l), err)
	}
}
