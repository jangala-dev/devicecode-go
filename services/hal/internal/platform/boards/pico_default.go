//go:build pico

package boards

import (
	_ "devicecode-go/services/hal/devices/led"
)

type Board struct {
	Name             string
	OnboardLED       int
	GPIOMin, GPIOMax int
}

var SelectedBoard = Board{
	Name:       "raspberrypi_pico",
	OnboardLED: 25,
	GPIOMin:    0,
	GPIOMax:    28,
}
