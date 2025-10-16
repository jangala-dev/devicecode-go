package types

// ------------------------
// Serial
// ------------------------

type Parity uint8

const (
	ParityNone Parity = iota
	ParityEven
	ParityOdd
)

type SerialSessionOpen struct {
	// Power-of-two sizes (bytes). Device will default if zero.
	RXSize int `json:"rx_size,omitempty"`
	TXSize int `json:"tx_size,omitempty"`
}

type SerialSessionClose struct{}

type SerialSetBaud struct {
	Baud uint32 `json:"baud"`
}

type SerialSetFormat struct {
	DataBits uint8  `json:"data_bits"`
	StopBits uint8  `json:"stop_bits"`
	Parity   Parity `json:"parity"`
}

type SerialSessionOpened struct {
	SessionID uint32 `json:"session_id"`
	RXHandle  uint32 `json:"rx_handle"`
	TXHandle  uint32 `json:"tx_handle"`
}

type SerialInfo struct {
	Bus  string `json:"bus"`
	Baud uint32 `json:"baud"` // 0 if unspecified
}
