// services/hal/internal/devices/gpio/adaptor_test.go

package gpio

import (
	"context"
	"testing"

	"devicecode-go/services/hal/internal/halcore"
	"devicecode-go/services/hal/internal/registry"
	"devicecode-go/types"
)

// ---- Test doubles ----

// fakeIRQPin implements both GPIOPin and IRQPin.
type fakeIRQPin struct {
	n      int
	level  bool
	config string
}

func (p *fakeIRQPin) ConfigureInput(_ halcore.Pull) error { p.config = "in"; return nil }
func (p *fakeIRQPin) ConfigureOutput(initial bool) error {
	p.config = "out"
	p.level = initial
	return nil
}
func (p *fakeIRQPin) Set(b bool)  { p.level = b }
func (p *fakeIRQPin) Get() bool   { return p.level }
func (p *fakeIRQPin) Toggle()     { p.level = !p.level }
func (p *fakeIRQPin) Number() int { return p.n }

// IRQPin methods (no-ops sufficient for builder path).
func (p *fakeIRQPin) SetIRQ(_ halcore.Edge, _ func()) error { return nil }
func (p *fakeIRQPin) ClearIRQ() error                       { return nil }

type fakePinFactory struct{}

func (fakePinFactory) ByNumber(n int) (halcore.GPIOPin, bool) { return &fakeIRQPin{n: n}, true }

// ---- Tests ----

func TestGPIOBuilderAndControl_Input(t *testing.T) {
	var pf fakePinFactory
	b := gpioBuilder{}
	out, err := b.Build(registry.BuildInput{
		DeviceID: "g1",
		Type:     "gpio",
		Pins:     pf,
		ParamsJSON: map[string]any{
			"pin":    12,
			"mode":   "input",
			"pull":   "up",
			"invert": true,
			"irq": map[string]any{
				"edge":        "rising",
				"debounce_ms": 5,
			},
		},
	})
	if err != nil {
		t.Fatalf("build failed: %v", err)
	}
	ad, ok := out.Adaptor.(*adaptor)
	if !ok {
		t.Fatalf("unexpected adaptor type: %T", out.Adaptor)
	}
	// Input supports "get".
	res, err := ad.Control("gpio", "get", nil)
	if err != nil {
		t.Fatalf("get failed: %v", err)
	}
	if _, ok := res.(types.GPIOGetReply); !ok {
		t.Fatalf("unexpected reply type: %T", res)
	}
	// Because the fake pin implements IRQPin and irq params were provided, IRQ should be requested.
	if out.IRQ == nil {
		t.Fatalf("expected IRQ request for input with irq params")
	}
}

func TestGPIOOutputControls(t *testing.T) {
	var pf fakePinFactory
	b := gpioBuilder{}
	out, err := b.Build(registry.BuildInput{
		DeviceID: "g2",
		Type:     "gpio",
		Pins:     pf,
		ParamsJSON: map[string]any{
			"pin":     3,
			"mode":    "output",
			"initial": true,
			"invert":  true, // logical true -> physical low initially
		},
	})
	if err != nil {
		t.Fatalf("build failed: %v", err)
	}
	ad := out.Adaptor.(*adaptor)

	// set
	if _, err := ad.Control("gpio", "set", types.GPIOSet{Level: true}); err != nil {
		t.Fatalf("set failed: %v", err)
	}
	// toggle
	if _, err := ad.Control("gpio", "toggle", nil); err != nil {
		t.Fatalf("toggle failed: %v", err)
	}
	// unsupported verb
	if _, err := ad.Control("gpio", "noop", nil); err == nil {
		t.Fatalf("expected error for unsupported verb")
	}
	// Trigger/Collect are unsupported for GPIO adaptor
	if _, err := ad.Trigger(context.Background()); err == nil {
		t.Fatalf("expected trigger unsupported")
	}
	if _, err := ad.Collect(context.Background()); err == nil {
		t.Fatalf("expected collect unsupported")
	}
}

func TestParseHelpers(t *testing.T) {
	if ParseEdge("rising") != halcore.EdgeRising ||
		ParseEdge("falling") != halcore.EdgeFalling ||
		ParseEdge("both") != halcore.EdgeBoth ||
		ParseEdge("none") != halcore.EdgeNone {
		t.Fatal("ParseEdge mapping wrong")
	}
	if parsePull("up") != halcore.PullUp || parsePull("down") != halcore.PullDown || parsePull("x") != halcore.PullNone {
		t.Fatal("parsePull mapping wrong")
	}
}
