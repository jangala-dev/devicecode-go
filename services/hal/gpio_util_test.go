package hal

import "testing"

func TestParsePull_ToPullString(t *testing.T) {
	if got := parsePull("up"); got != PullUp {
		t.Fatalf("parsePull(up) got %v", got)
	}
	if got := parsePull("down"); got != PullDown {
		t.Fatalf("parsePull(down) got %v", got)
	}
	if got := parsePull("none"); got != PullNone {
		t.Fatalf("parsePull(none) got %v", got)
	}
	if got := parsePull("unknown"); got != PullNone {
		t.Fatalf("parsePull(unknown) got %v", got)
	}

	if s := toPullString(PullUp); s != "up" {
		t.Fatalf("toPullString(PullUp)=%q", s)
	}
	if s := toPullString(PullDown); s != "down" {
		t.Fatalf("toPullString(PullDown)=%q", s)
	}
	if s := toPullString(PullNone); s != "none" {
		t.Fatalf("toPullString(PullNone)=%q", s)
	}
}

func TestBoolToInt_EdgeToString_AsString(t *testing.T) {
	if boolToInt(true) != 1 || boolToInt(false) != 0 {
		t.Fatalf("boolToInt failed")
	}
	if edgeToString(EdgeRising) != "rising" ||
		edgeToString(EdgeFalling) != "falling" ||
		edgeToString(EdgeBoth) != "both" ||
		edgeToString(EdgeNone) != "none" {
		t.Fatalf("edgeToString failed")
	}
	if asString(123) != "" || asString("x") != "x" {
		t.Fatalf("asString failed")
	}
}
