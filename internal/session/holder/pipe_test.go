package holder

import (
	"bytes"
	"os"
	"syscall"
	"testing"
	"time"
)

// TestHolderPipeMode: a pipe child gets no terminal; the F frame carries its
// stdin, O frames its stdout, stdout is spooled whole and stderr stays out of
// the stream (SPEC 8 "Structured sessions").
func TestHolderPipeMode(t *testing.T) {
	home := tempHome(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	st, err := Start(exe, Spec{Session: "abcd1234", Home: home, Path: "/bin/sh",
		Args: []string{"sh", "-c", `echo '{"seq":1}'; echo to-stderr >&2; if [ -t 0 ]; then echo tty; fi; exec cat`},
		Dir:  home, Env: []string{"PATH=/usr/bin:/bin"}, Pipe: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-st.ChildPID, syscall.SIGKILL)
		waitExit(t, st)
	})
	c, err := Dial(home, "abcd1234")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if !c.Hello.Pipe {
		t.Fatalf("hello: %+v", c.Hello)
	}
	f, err := c.Adopt(0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	until(t, c, `{"seq":1}`)
	if _, err := f.Write([]byte(`{"op":"turn"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	out, _ := until(t, c, `{"op":"turn"}`)
	if bytes.Contains([]byte(out), []byte("to-stderr")) || bytes.Contains([]byte(out), []byte("tty")) {
		t.Fatalf("stream: %q", out)
	}
	// A daemon going away never closes the child's stdin: the holder keeps it.
	_ = f.Close()
	_ = c.Close()
	c, err = Dial(home, "abcd1234")
	if err != nil {
		t.Fatal(err)
	}
	f, err = c.Adopt(0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("again\n")); err != nil {
		t.Fatal(err)
	}
	until(t, c, "again")
	if err := c.Signal("TERM", "test"); err != nil {
		t.Fatal(err)
	}
	if _, x := until(t, c, ""); x == nil || x.Signal != "TERM" {
		t.Fatalf("exit: %+v", x)
	}
	_ = c.Ack()
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, _ := os.ReadFile(PipeEventsPath(home, "abcd1234"))
		e, _ := os.ReadFile(PipeErrorPath(home, "abcd1234"))
		if string(b) == "{\"seq\":1}\n{\"op\":\"turn\"}\nagain\n" && string(e) == "to-stderr\n" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("spool %q, stderr %q", b, e)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
