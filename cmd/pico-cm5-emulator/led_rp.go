//go:build tinygo && (rp2040 || rp2350)

package main

import (
	"machine"
	"time"
)

var statusLED = machine.LED

func ledInit() {
	statusLED.Configure(machine.PinConfig{Mode: machine.PinOutput})
	statusLED.Low()
}

func ledOn()  { statusLED.High() }
func ledOff() { statusLED.Low() }

func ledPassLoop() {
	for {
		statusLED.High()
		time.Sleep(1800 * time.Millisecond)
		statusLED.Low()
		time.Sleep(200 * time.Millisecond)
	}
}

func ledFailLoop() {
	for {
		statusLED.High()
		time.Sleep(120 * time.Millisecond)
		statusLED.Low()
		time.Sleep(120 * time.Millisecond)
	}
}
