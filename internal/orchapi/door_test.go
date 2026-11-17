package orchapi

import "testing"

func TestDoorDropsBothNames(t *testing.T) {
	for _, k := range []string{"Zm-As", "X-Zm-Edge-Secret", "Potluck-As", "potluck-protocol", "X-Potluck-Edge-Secret"} {
		if !DoorDropHeader(k) {
			t.Errorf("%s reaches the Mezze app", k)
		}
	}
	if DoorDropHeader("X-Github-Event") {
		t.Error("a delivery's own header dropped")
	}
}
