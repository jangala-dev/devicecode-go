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

func PtrI32(v int32) *int32   { return &v }
func PtrU32(v uint32) *uint32 { return &v }
func PtrU16(v uint16) *uint16 { return &v }
