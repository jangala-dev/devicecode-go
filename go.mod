module devicecode-go

go 1.25.1

require (
	ab-bringup v0.0.0
	github.com/jangala-dev/tinygo-uartx v0.0.0-20251028085354-58b6258234b3
	golang.org/x/exp v0.0.0-20251002181428-27f1f14c8bb9
	tinygo.org/x/drivers v0.33.0
)

require github.com/google/shlex v0.0.0-20191202100458-e7afc7fbc510 // indirect

replace ab-bringup => ../pico2-a-b

replace github.com/jangala-dev/tinygo-uartx => ../tinygo-uartx
