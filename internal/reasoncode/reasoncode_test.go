package reasoncode

import "testing"

func TestRoundTrip(t *testing.T) {
	c := New("closeBlackout", "minutesLeft", "2", "blackout", "30.0")
	if !c.Valid() {
		t.Fatalf("%+v not valid", c)
	}
	got, ok := Parse(*c.JSON())
	if !ok || got.Key != c.Key || got.Args["blackout"] != "30.0" {
		t.Fatalf("round trip: %+v %v", got, ok)
	}
	if (Code{}).JSON() != nil {
		t.Fatal("zero code must store NULL")
	}
	if _, ok := Parse(""); ok {
		t.Fatal("empty parsed")
	}
	if New("halted").Valid() != true || New("closeBlackout", "minutesLeft", "2").Valid() || New("nope").Valid() {
		t.Fatal("Valid")
	}
}
