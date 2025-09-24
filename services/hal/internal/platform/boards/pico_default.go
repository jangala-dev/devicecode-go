//go:build pico

package boards

var SelectedBoard = Board{
	Name:    "raspberrypi_pico",
	GPIOMin: 0,
	GPIOMax: 28,
	I2C:     []string{"i2c0", "i2c1"},
	SPI:     nil, // add when we expose SPI owners
	UART:    []string{"uart0", "uart1"},
	Defaults: struct {
		I2C0_SDA, I2C0_SCL int
		I2C1_SDA, I2C1_SCL int
		UART0_TX, UART0_RX int
		UART1_TX, UART1_RX int
	}{
		// RP2040 default pins (GPIO numbers)
		I2C0_SDA: 4, I2C0_SCL: 5,
		I2C1_SDA: 2, I2C1_SCL: 3,
		UART0_TX: 0, UART0_RX: 1,
		UART1_TX: 8, UART1_RX: 9,
	},
}
