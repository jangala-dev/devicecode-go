//go:build tinygo && rp2350 && fabric_uart_hwtest

package updater

// streamedStage is shared by the production RP2350 prestage path and the
// fabric_uart_hwtest prestage sink.  The production definition lives in
// prestage_tinygo.go, which is deliberately excluded under fabric_uart_hwtest
// so that the hardware UART/Fabric interconnection test does not write the
// inactive A/B flash slot.
type streamedStage struct {
	Version       string
	BuildID       string
	ImageID       string
	Length        uint32
	PayloadSHA256 string
}
