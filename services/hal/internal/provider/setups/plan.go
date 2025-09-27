package setups

// ResourcePlan specifies wiring and operating parameters chosen by a setup.
// Providers consume this plan to instantiate resource owners.
type ResourcePlan struct {
	I2C  []I2CPlan
	UART []UARTPlan
	// SPI, CAN, etc. can be added later in the same manner.
}

type I2CPlan struct {
	ID  string // e.g. "i2c0"
	SDA int    // GPIO number
	SCL int    // GPIO number
	Hz  uint32 // bus frequency
}

type UARTPlan struct {
	ID   string // e.g. "uart0"
	TX   int    // GPIO number
	RX   int    // GPIO number
	Baud uint32 // initial baud (format can be added later)
}
