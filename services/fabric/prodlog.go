package fabric

import (
	"devicecode-go/utilities/diag"
	"devicecode-go/x/strconvx"
)

func logFabricXferBegin(id, target string, size uint32) {
	diag.Println("[fabric-xfer]", "ev", "begin", "xfer_id", id, "target", target, "size", strconvx.Itoa(int(size)))
}

func logFabricXferProgress(id string, next, size, chunks uint32, writeMS int) {
	diag.Println("[fabric-xfer]", "ev", "progress", "xfer_id", id, "next", strconvx.Itoa(int(next)), "size", strconvx.Itoa(int(size)), "chunks", strconvx.Itoa(int(chunks)), "write_ms", strconvx.Itoa(writeMS))
}

func logFabricXferReject(id, reason string, offset, expected uint32, rawLen, encodedLen, lineLen int, want, got string) {
	diag.Println("[fabric-xfer]", "ev", "chunk_reject", "xfer_id", id, "reason", reason, "offset", strconvx.Itoa(int(offset)), "expected", strconvx.Itoa(int(expected)), "raw_len", strconvx.Itoa(rawLen), "encoded_len", strconvx.Itoa(encodedLen), "line_len", strconvx.Itoa(lineLen), "want", want, "got", got)
}

func logFabricXferAbort(id, reason string, next, size, chunks uint32, idleRetries uint8) {
	diag.Println("[fabric-xfer]", "ev", "abort", "xfer_id", id, "reason", reason, "next", strconvx.Itoa(int(next)), "size", strconvx.Itoa(int(size)), "chunks", strconvx.Itoa(int(chunks)), "idle_retries", strconvx.Itoa(int(idleRetries)))
}

func logFabricXferDone(id string, bytes uint32, chunks uint32) {
	diag.Println("[fabric-xfer]", "ev", "done", "xfer_id", id, "bytes", strconvx.Itoa(int(bytes)), "chunks", strconvx.Itoa(int(chunks)))
}

func logFabricXferStageReply(id string, ok bool, reason string, bytes uint32, generation uint64) {
	diag.Println("[fabric-xfer]", "ev", "stage_reply", "xfer_id", id, "ok", ok, "reason", reason, "bytes", strconvx.Itoa(int(bytes)), "generation", strconvx.Itoa64(int64(generation)))
}

func logFabricRXLoss(reason string, next, size, chunks uint32, rxLines, rxLineTooLong, rxBadJSON uint64, lineLen int, frameType, xferID, err string) {
	diag.Println("[fabric-rx]", "ev", "loss", "reason", reason, "xfer_id", xferID, "next", strconvx.Itoa(int(next)), "size", strconvx.Itoa(int(size)), "chunks", strconvx.Itoa(int(chunks)), "rx_lines", strconvx.Itoa64(int64(rxLines)), "rx_line_too_long", strconvx.Itoa64(int64(rxLineTooLong)), "rx_bad_json", strconvx.Itoa64(int64(rxBadJSON)), "line_len", strconvx.Itoa(lineLen), "frame_type", frameType, "err", err)
}
