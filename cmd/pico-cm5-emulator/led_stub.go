//go:build !tinygo || !(rp2040 || rp2350)

package main

func ledInit()     {}
func ledOn()       {}
func ledOff()      {}
func ledPassLoop() { select {} }
func ledFailLoop() { select {} }
