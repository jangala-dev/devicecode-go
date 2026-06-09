package fabric

// FabricCounters is the compact normal-build diagnostic surface for the MCU
// Fabric link. Counters are updated by the session reactor and published with
// retained link state; they replace per-frame/per-chunk logging in release
// builds.
type FabricCounters struct {
	RXLines               uint64 `json:"rx_lines"`
	RXLineTooLong         uint64 `json:"rx_line_too_long"`
	RXBadJSON             uint64 `json:"rx_bad_json"`
	RXFrames              uint64 `json:"rx_frames"`
	TXFrames              uint64 `json:"tx_frames"`
	TransferBegins        uint64 `json:"transfer_begins"`
	TransferChunks        uint64 `json:"transfer_chunks"`
	TransferBytes         uint64 `json:"transfer_bytes"`
	TransferDecodeErrors  uint64 `json:"transfer_decode_errors"`
	TransferDigestErrors  uint64 `json:"transfer_digest_errors"`
	TransferOffsetRetries uint64 `json:"transfer_offset_retries"`
	TransferAborts        uint64 `json:"transfer_aborts"`
	TransferCompletions   uint64 `json:"transfer_completions"`
}
