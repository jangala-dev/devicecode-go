package hal

import (
	"context"
	"testing"
)

// ---- fakes ----

type fakePin struct {
	level   bool
	mode    string // "input" or "output"
	pull    Pull
	num     int
	toggles int
}

func (p *fakePin) ConfigureInput(pull Pull) error { p.mode, p.pull = "input", pull; return nil }
func (p *fakePin) ConfigureOutput(initial bool) error {
	p.mode = "output"
	p.level = initial
	return nil
}
func (p *fakePin) Set(level bool) { p.level = level }
func (p *fakePin) Get() bool      { return p.level }
func (p *fakePin) Toggle()        { p.level = !p.level; p.toggles++ }
func (p *fakePin) Number() int    { return p.num }

// ---- tests ----

func TestGPIOAdaptor_Capabilities(t *testing.T) {
	fp := &fakePin{num: 7}
	ad := NewGPIOAdaptor("gpio1", fp, GPIOParams{Mode: "output", Pull: "down", Invert: true})

	caps := ad.Capabilities()
	if len(caps) != 1 {
		t.Fatalf("expected 1 capability, got %d", len(caps))
	}
	if caps[0].Kind != "gpio" {
		t.Fatalf("expected kind 'gpio', got %q", caps[0].Kind)
	}
	info := caps[0].Info
	if info["pin"] != 7 {
		t.Fatalf("info.pin: want 7, got %v", info["pin"])
	}
	if info["mode"] != "output" {
		t.Fatalf("info.mode: want output, got %v", info["mode"])
	}
	if info["invert"] != true {
		t.Fatalf("info.invert: want true, got %v", info["invert"])
	}
	if info["pull"] != "down" {
		t.Fatalf("info.pull: want down, got %v", info["pull"])
	}
}

func TestGPIOAdaptor_ConfigureInput(t *testing.T) {
	fp := &fakePin{}
	ad := NewGPIOAdaptor("g2", fp, GPIOParams{})

	res, err := ad.Control("gpio", "configure_input", map[string]any{"pull": "up"})
	if err != nil {
		t.Fatalf("configure_input error: %v", err)
	}
	if fp.mode != "input" || fp.pull != PullUp {
		t.Fatalf("pin not configured to input/pullup; mode=%s pull=%v", fp.mode, fp.pull)
	}
	m := res.(map[string]any)
	if m["mode"] != "input" || m["pull"] != "up" {
		t.Fatalf("reply mismatch: %v", m)
	}
}

func TestGPIOAdaptor_ConfigureOutput_SetGetToggle_Invert(t *testing.T) {
	fp := &fakePin{}
	ad := NewGPIOAdaptor("g3", fp, GPIOParams{Invert: true})

	// initial high (logical) with invert=true -> physical low
	if _, err := ad.Control("gpio", "configure_output", map[string]any{"initial": 1}); err != nil {
		t.Fatalf("configure_output error: %v", err)
	}
	if fp.mode != "output" {
		t.Fatalf("pin mode not output")
	}
	if fp.level != false {
		t.Fatalf("physical level after initial high with invert should be low (false), got %v", fp.level)
	}

	// set high (logical) -> physical low (false)
	if _, err := ad.Control("gpio", "set", map[string]any{"level": 1}); err != nil {
		t.Fatalf("set error: %v", err)
	}
	if fp.level != false {
		t.Fatalf("physical level expected false, got %v", fp.level)
	}

	// get should report logical high (1)
	res, err := ad.Control("gpio", "get", nil)
	if err != nil {
		t.Fatalf("get error: %v", err)
	}
	if lvl, _ := res.(map[string]any)["level"].(int); lvl != 1 {
		t.Fatalf("get returned level=%v, want 1", lvl)
	}

	// toggle flips physical; with invert=true logical view flips accordingly
	if _, err := ad.Control("gpio", "toggle", nil); err != nil {
		t.Fatalf("toggle error: %v", err)
	}
	if fp.level != true {
		t.Fatalf("physical level after toggle should be true, got %v", fp.level)
	}
}

func TestGPIOAdaptor_Unsupported(t *testing.T) {
	fp := &fakePin{}
	ad := NewGPIOAdaptor("g4", fp, GPIOParams{})

	if _, err := ad.Control("gpio", "no_such_method", nil); err == nil {
		t.Fatalf("expected error for unsupported control method")
	}

	if _, err := ad.Trigger(context.Background()); err == nil {
		t.Fatalf("Trigger should be unsupported")
	}
	if _, err := ad.Collect(context.Background()); err == nil {
		t.Fatalf("Collect should be unsupported")
	}
}
