package holder

import "testing"

func TestRing(t *testing.T) {
	r := NewRing(8)
	_, _ = r.Write([]byte("0123456789"))
	data, from, to, _, _ := r.Next(0, 100)
	if string(data) != "23456789" || from != 2 || to != 10 {
		t.Fatalf("%q %d %d", data, from, to)
	}
	_, _ = r.Write([]byte("ab"))
	data, from, _, _, _ = r.Next(9, 100)
	if string(data) != "9ab" || from != 9 {
		t.Fatalf("%q %d", data, from)
	}
}

// TestRingWriteAt: a mirror keeps the writer's offsets: overlaps are
// skipped, a gap moves the end.
func TestRingWriteAt(t *testing.T) {
	r := NewRing(8)
	r.WriteAt(100, nil) // a mirror starting at 100 holds nothing before it
	if s, e := r.Bounds(); s != 100 || e != 100 {
		t.Fatalf("bounds %d %d", s, e)
	}
	r.WriteAt(100, []byte("abc"))
	r.WriteAt(101, []byte("bcd")) // overlaps two bytes
	r.WriteAt(98, []byte("xy"))   // all old
	data, from, to, _, _ := r.Next(0, 100)
	if string(data) != "abcd" || to != 104 || from != 100 {
		t.Fatalf("%q %d %d", data, from, to)
	}
	r.WriteAt(110, []byte("z")) // a gap
	if _, e := r.Bounds(); e != 111 {
		t.Fatalf("end %d", e)
	}
}

// TestRingFloor: a mirror that starts late, or skips a gap, never serves
// the bytes it never got (they would be NULs).
func TestRingFloor(t *testing.T) {
	r := NewRing(16)
	r.WriteAt(0, nil)
	r.WriteAt(10, []byte("abc"))
	data, from, to, _, _ := r.Next(0, 100)
	if string(data) != "abc" || from != 10 || to != 13 {
		t.Fatalf("%q %d %d", data, from, to)
	}
	r.WriteAt(20, []byte("xy")) // a gap inside the buffer's reach
	if s, _ := r.Bounds(); s != 20 {
		t.Fatalf("start after a gap: %d", s)
	}
	data, _, _, _, _ = r.Next(0, 100)
	if string(data) != "xy" {
		t.Fatalf("after the gap: %q", data)
	}
}
