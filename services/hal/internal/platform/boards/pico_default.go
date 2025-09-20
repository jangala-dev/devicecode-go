//go:build rp2040 && board_pico_default

package boards

// Minimal descriptor for Pico bring-up; onboard LED is GP25.
type Descriptor struct {
	Name string
	LED  int
}

var Selected = Descriptor{
	Name: "pico_default",
	LED:  25,
}
