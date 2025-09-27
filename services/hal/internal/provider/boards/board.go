package boards

// Board describes what the PCB/SoC can do (controllers present, GPIO range).
// It must not include wiring choices (pins) or operating parameters (clock rates).
type Board struct {
	Name             string
	GPIOMin, GPIOMax int

	// Controllers present (identities only; e.g. "i2c0", "i2c1", "uart0", "uart1").
	I2C  []string
	SPI  []string
	UART []string

	// Optional recommended default aliases for convenience in setups/tools.
	// These are plain GPIO numbers; mapping to machine.Pin happens in the provider.
	Defaults struct {
		I2C0_SDA, I2C0_SCL int
		I2C1_SDA, I2C1_SCL int
		UART0_TX, UART0_RX int
		UART1_TX, UART1_RX int
	}
}
