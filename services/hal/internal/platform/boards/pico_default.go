//go:build pico && board_pico_default

package boards

import (
	_ "devicecode-go/services/hal/devices/led"
)

// Minimal descriptor for Pico bring-up; onboard LED is GP25.
type Descriptor struct {
	Name string
	LED  int
}

var Selected = Descriptor{
	Name: "pico_default",
	LED:  25,
}
