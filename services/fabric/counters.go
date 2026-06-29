package fabric

// FabricCounters is the compact normal-build diagnostic surface for the MCU
// Fabric link. Counters are updated by the session reactor and published with
// retained link state; production firmware uses these counters plus sparse
// lifecycle/error logs rather than per-frame tracing.
type FabricCounters struct {
	RXLines                 uint64 `json:"rx_lines"`
	RXLineTooLong           uint64 `json:"rx_line_too_long"`
	RXBadJSON               uint64 `json:"rx_bad_json"`
	RXFrames                uint64 `json:"rx_frames"`
	TXFrames                uint64 `json:"tx_frames"`
	TransferBegins          uint64 `json:"transfer_begins"`
	TransferChunks          uint64 `json:"transfer_chunks"`
	TransferBytes           uint64 `json:"transfer_bytes"`
	TransferDecodeErrors    uint64 `json:"transfer_decode_errors"`
	TransferDigestErrors    uint64 `json:"transfer_digest_errors"`
	TransferOffsetRetries   uint64 `json:"transfer_offset_retries"`
	TransferBadFrameRetries uint64 `json:"transfer_bad_frame_retries"`
	TransferIdleRetries     uint64 `json:"transfer_idle_retries"`
	TransferChunkRejects    uint64 `json:"transfer_chunk_rejects"`
	TransferWriteErrors     uint64 `json:"transfer_write_errors"`
	TransferCommitErrors    uint64 `json:"transfer_commit_errors"`
	TransferAborts          uint64 `json:"transfer_aborts"`
	TransferCompletions     uint64 `json:"transfer_completions"`
}
