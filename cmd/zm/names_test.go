package main

import "testing"

func TestSplitOwnerName(t *testing.T) {
	for in, want := range map[string][2]string{"@ana/harbor": {"ana", "harbor"}, "acme/web-app": {"acme", "web-app"}, " @a/b ": {"a", "b"}} {
		o, n, err := splitOwnerName(in)
		if err != nil || o != want[0] || n != want[1] {
			t.Errorf("%q = %q %q %v", in, o, n, err)
		}
	}
	for _, bad := range []string{"", "@ana", "ana/", "/x", "@a/b/c"} {
		if _, _, err := splitOwnerName(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
