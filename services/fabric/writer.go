package fabric

import "errors"

// Outbound frame scheduler — control / rpc / bulk lanes per
// devicecode-lua@2c88090 src/services/fabric/writer.lua. Control bypasses
// fairness and drains first; rpc and bulk share remaining bandwidth via
// weighted round-robin (defaults rpc_quantum=4, bulk_quantum=1).
//
// Lane assignment for outbound MCU frames mirrors protocol.lua's
// FRAME_CLASS map. The MCU never originates xfer_chunk so the bulk lane
// is currently unused on the MCU side; it is wired in for symmetry and
// for future MCU-originated bulk-frame users.

type lane uint8

const (
	laneControl lane = iota
	laneRPC
	laneBulk
)

// txLane is a single FIFO of pending wire frames.
type txLane struct {
	frames [][]byte
}

func (l *txLane) push(data []byte) { l.frames = append(l.frames, data) }
func (l *txLane) len() int         { return len(l.frames) }
func (l *txLane) pop() []byte {
	f := l.frames[0]
	l.frames = l.frames[1:]
	if len(l.frames) == 0 {
		l.frames = nil
	}
	return f
}

// enqueueFrame routes data into the lane and immediately drains the
// writer in priority order. With a single producer goroutine the queue
// is normally drained empty before the next caller, but the lane
// discipline kicks in when multiple frames are queued in a single tick
// (e.g. drainExports + drainOutbound generating frames back-to-back).
func (s *session) enqueueFrame(l lane, data []byte) bool {
	s.lane(l).push(data)
	if fabricTraceEnabled {
		println(
			"[fabric]", "sid", s.localSID,
			"enqueue_frame",
			"lane", laneName(l),
			"type", protoType(data),
			"len", len(data),
			"q_control", s.txControl.len(),
			"q_rpc", s.txRPC.len(),
			"q_bulk", s.txBulk.len(),
		)
	}
	return s.flushWriter()
}

func (s *session) lane(l lane) *txLane {
	switch l {
	case laneControl:
		return &s.txControl
	case laneRPC:
		return &s.txRPC
	case laneBulk:
		return &s.txBulk
	default:
		return &s.txRPC
	}
}

func laneName(l lane) string {
	switch l {
	case laneControl:
		return "control"
	case laneRPC:
		return "rpc"
	case laneBulk:
		return "bulk"
	default:
		return "unknown"
	}
}

// flushWriter writes queued frames to the transport in priority order:
// 1. drain controlQ fully (no fairness),
// 2. weighted RR between rpcQ and bulkQ until both empty.
// Returns false on transport-write failure (link torn down).
func (s *session) flushWriter() bool {
	rpcQ, bulkQ := s.cfg.RPCQuantum, s.cfg.BulkQuantum
	// Defensive: guarantee forward progress even if a caller bypasses
	// applyDefaults (e.g. unit tests constructing session{} directly).
	// Without this, a zero quantum would spin the outer loop forever.
	if rpcQ <= 0 {
		rpcQ = 1
	}
	if bulkQ <= 0 {
		bulkQ = 1
	}
	for s.txControl.len() > 0 {
		if !s.writeFrame(laneControl, s.txControl.pop()) {
			return false
		}
	}
	for s.txRPC.len() > 0 || s.txBulk.len() > 0 {
		for i := 0; i < rpcQ && s.txRPC.len() > 0; i++ {
			if !s.writeFrame(laneRPC, s.txRPC.pop()) {
				return false
			}
		}
		for i := 0; i < bulkQ && s.txBulk.len() > 0; i++ {
			if !s.writeFrame(laneBulk, s.txBulk.pop()) {
				return false
			}
		}
	}
	return true
}

// writeFrame is the actual transport write. Mirrors what the prior
// sendFrame did inline; isolated so flushWriter can call it per-frame.
func (s *session) writeFrame(l lane, data []byte) bool {
	if len(data) > 0 && data[len(data)-1] == '\n' {
		data = data[:len(data)-1]
	}
	if fabricTraceEnabled {
		println(
			"[fabric]", "sid", s.localSID,
			"tx_frame",
			"lane", laneName(l),
			"type", protoType(data),
			"len", len(data),
			"line", tracePreview(data),
		)
	}
	if err := s.tr.WriteLine(data); err != nil {
		if errors.Is(err, ErrLineTooLong) {
			s.log("oversized write dropped")
			return true
		}
		s.handleLinkDown(reasonTransportWrite, err.Error())
		return false
	}
	s.markTx()
	return true
}
