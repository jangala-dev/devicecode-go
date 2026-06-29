//go:build !tinygo || fabric_uart_selftest

package fabric

import (
	"context"
	"encoding/base64"
	"errors"
	"strconv"
	"time"

	"devicecode-go/bus"
	"devicecode-go/services/updater"
	"devicecode-go/x/shmring"
	"devicecode-go/x/xxhash"
)

// UARTSelfTestOptions describes an opt-in in-process Fabric transfer test. It
// uses the same newline JSONL and shmring transport shape as the UART session,
// but cross-connects the rings in memory so a board can exercise Fabric and the
// updater stage-controller boundary without an external serial peer.
type UARTSelfTestOptions struct {
	Conn            *bus.Connection
	StageController StageController
	PayloadSize     int
	ChunkSize       int
	Timeout         time.Duration
}

type UARTSelfTestResult struct {
	PayloadSize uint32
	ChunkSize   uint32
	Digest      string
	XferID      string
}

func (r UARTSelfTestResult) OK() bool { return r.XferID != "" && r.PayloadSize > 0 }

const defaultSelfTestPayloadSize = 1024
const defaultSelfTestChunkSize = 256
const defaultSelfTestTimeout = 10 * time.Second

// Keep the self-test's large scratch areas out of goroutine stacks. The normal
// hardware path already keeps the MCU Fabric buffer package-level in the
// Reactor. The self-test is single-shot and opt-in, so package-level scratch is
// acceptable and avoids invalidating the 3 KB stack gate.
var selfTestMCUBuffers FabricBuffers
var selfTestPeerLine [maxLineLen]byte
var selfTestB64 [maxChunkBase64Len]byte

// RunUARTSelfTest starts an MCU Fabric session and a tiny in-process CM5 peer
// connected by cross-wired shmring transports. It performs prepare-update and a
// transfer to updater/main, then stops before commit-update/reboot. This is a
// hardware smoke gate for Fabric framing and the updater stage-controller seam;
// it is not a production A/B flash test.
func RunUARTSelfTest(ctx context.Context, opts UARTSelfTestOptions) (UARTSelfTestResult, error) {
	if opts.Conn == nil {
		return UARTSelfTestResult{}, errors.New("missing_bus_connection")
	}
	if opts.StageController == nil {
		return UARTSelfTestResult{}, errors.New("missing_stage_controller")
	}
	payloadSize := opts.PayloadSize
	if payloadSize <= 0 {
		payloadSize = defaultSelfTestPayloadSize
	}
	chunkSize := opts.ChunkSize
	if chunkSize <= 0 {
		chunkSize = defaultSelfTestChunkSize
	}
	if chunkSize > payloadSize {
		chunkSize = payloadSize
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultSelfTestTimeout
	}

	// Cross-wired UART-shaped rings:
	//   peer TX -> MCU RX on a
	//   MCU TX  -> peer RX on b
	// The rings only carry this self-test's small line frames, so 2048 bytes per
	// direction is enough and avoids permanently retaining another pair of full
	// UART-sized rings on the MCU.
	a := shmring.New(2048)
	b := shmring.New(2048)
	mcuTr := NewShmringTransportWithBuffers(a, b, &selfTestMCUBuffers)
	peer := newUARTSelfTestPeer(b, a, "cm5-selftest-sid")

	testCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	defer mcuTr.Close()
	defer peer.Close()
	go func() {
		<-testCtx.Done()
		_ = mcuTr.Close()
		_ = peer.Close()
	}()

	fabricDone := make(chan struct{})
	go func() {
		defer close(fabricDone)
		RunWithOptions(testCtx, mcuTr, opts.Conn, "mcu", "bigbox-cm5", DefaultLinkConfig(), RunOptions{Buffers: &selfTestMCUBuffers, StageController: opts.StageController})
	}()

	if err := peer.writeHello(); err != nil {
		return UARTSelfTestResult{}, err
	}
	ackLine, err := peer.waitType(msgHelloAck, "")
	if err != nil {
		return UARTSelfTestResult{}, err
	}
	if protoTopString(ackLine, "node") != "mcu" || protoTopString(ackLine, "sid") == "" {
		return UARTSelfTestResult{}, errors.New("bad_hello_ack")
	}

	prepareID := "selftest-prepare-1"
	if err := peer.writePrepare(prepareID); err != nil {
		return UARTSelfTestResult{}, err
	}
	replyLine, err := peer.waitReply(prepareID)
	if err != nil {
		return UARTSelfTestResult{}, err
	}
	ok, okField := protoTopBool(replyLine, "ok")
	if !okField || !ok {
		errText := protoTopString(replyLine, "err")
		if errText != "" {
			return UARTSelfTestResult{}, errors.New("prepare_failed:" + errText)
		}
		return UARTSelfTestResult{}, errors.New("prepare_failed")
	}

	payload := selfTestPayload(payloadSize)
	digest := selfTestXXHash(payload)
	xferID := "selftest-xfer-1"
	if err := peer.writeXferBegin(xferID, uint32(len(payload)), digest); err != nil {
		return UARTSelfTestResult{}, err
	}
	if _, err := peer.waitType(msgXferReady, xferID); err != nil {
		return UARTSelfTestResult{}, err
	}
	if err := peer.waitNeed(xferID, 0); err != nil {
		return UARTSelfTestResult{}, err
	}

	for off := 0; off < len(payload); off += chunkSize {
		end := off + chunkSize
		if end > len(payload) {
			end = len(payload)
		}
		chunk := payload[off:end]
		if err := peer.writeXferChunk(xferID, uint32(off), chunk); err != nil {
			return UARTSelfTestResult{}, err
		}
		if err := peer.waitNeed(xferID, uint32(end)); err != nil {
			return UARTSelfTestResult{}, err
		}
	}
	if err := peer.writeXferCommit(xferID, uint32(len(payload)), digest); err != nil {
		return UARTSelfTestResult{}, err
	}
	if _, err := peer.waitType(msgXferDone, xferID); err != nil {
		return UARTSelfTestResult{}, err
	}

	cancel()
	select {
	case <-fabricDone:
	case <-time.After(200 * time.Millisecond):
	}

	return UARTSelfTestResult{PayloadSize: uint32(len(payload)), ChunkSize: uint32(chunkSize), Digest: digest, XferID: xferID}, nil
}

type uartSelfTestPeer struct {
	rx      *shmring.Ring
	tx      *shmring.Ring
	ctx     context.Context
	cancel  context.CancelFunc
	lineBuf *[maxLineLen]byte
	n       int
	over    bool
	sid     string
}

func newUARTSelfTestPeer(rx, tx *shmring.Ring, sid string) *uartSelfTestPeer {
	ctx, cancel := context.WithCancel(context.Background())
	return &uartSelfTestPeer{rx: rx, tx: tx, ctx: ctx, cancel: cancel, lineBuf: &selfTestPeerLine, sid: sid}
}

func (p *uartSelfTestPeer) Close() error {
	p.cancel()
	return nil
}

func (p *uartSelfTestPeer) writeHello() error {
	return p.writeLineBytes([]byte(`{"type":"hello","proto":"fabric-jsonl/1","sid":"cm5-selftest-sid","node":"bigbox-cm5"}`))
}

func (p *uartSelfTestPeer) writePrepare(id string) error {
	b := make([]byte, 0, 192)
	b = append(b, `{"type":"call","id":"`...)
	b = appendJSONString(b, id)
	b = append(b, `","topic":["cap","self","updater","main","rpc","prepare-update"],"payload":{"job_id":"selftest-job","target":"mcu","expected_image_id":"hwtest-image"}}`...)
	return p.writeLineBytes(b)
}

func (p *uartSelfTestPeer) writeXferBegin(id string, size uint32, digest string) error {
	b := make([]byte, 0, 192)
	b = append(b, `{"type":"xfer_begin","xfer_id":"`...)
	b = appendJSONString(b, id)
	b = append(b, `","target":"`...)
	b = appendJSONString(b, updater.TargetUpdaterMain)
	b = append(b, `","size":`...)
	b = strconv.AppendUint(b, uint64(size), 10)
	b = append(b, `,"digest_alg":"`...)
	b = appendJSONString(b, updater.DigestAlgXXHash32)
	b = append(b, `","digest":"`...)
	b = appendJSONString(b, digest)
	b = append(b, `","meta":{"source":"mcu-selftest"}}`...)
	return p.writeLineBytes(b)
}

func (p *uartSelfTestPeer) writeXferChunk(id string, off uint32, chunk []byte) error {
	n := base64.RawURLEncoding.EncodedLen(len(chunk))
	if n > len(selfTestB64) {
		return ErrLineTooLong
	}
	base64.RawURLEncoding.Encode(selfTestB64[:n], chunk)
	chunkDigest := selfTestXXHash(chunk)
	b := make([]byte, 0, 160+n)
	b = append(b, `{"type":"xfer_chunk","xfer_id":"`...)
	b = appendJSONString(b, id)
	b = append(b, `","offset":`...)
	b = strconv.AppendUint(b, uint64(off), 10)
	b = append(b, `,"data":"`...)
	b = append(b, selfTestB64[:n]...)
	b = append(b, `","chunk_digest":"`...)
	b = appendJSONString(b, chunkDigest)
	b = append(b, `"}`...)
	return p.writeLineBytes(b)
}

func (p *uartSelfTestPeer) writeXferCommit(id string, size uint32, digest string) error {
	b := make([]byte, 0, 144)
	b = append(b, `{"type":"xfer_commit","xfer_id":"`...)
	b = appendJSONString(b, id)
	b = append(b, `","size":`...)
	b = strconv.AppendUint(b, uint64(size), 10)
	b = append(b, `,"digest_alg":"`...)
	b = appendJSONString(b, updater.DigestAlgXXHash32)
	b = append(b, `","digest":"`...)
	b = appendJSONString(b, digest)
	b = append(b, `"}`...)
	return p.writeLineBytes(b)
}

func (p *uartSelfTestPeer) waitType(wantType, wantXfer string) ([]byte, error) {
	for {
		line, err := p.readLine()
		if err != nil {
			return nil, err
		}
		mt := protoType(line)
		if mt == msgXferAbort {
			id := protoTopString(line, "xfer_id")
			if wantXfer == "" || id == wantXfer {
				return nil, errors.New("xfer_abort:" + protoTopString(line, "err"))
			}
		}
		if mt != wantType {
			continue
		}
		if wantXfer == "" || protoTopString(line, "xfer_id") == wantXfer {
			return line, nil
		}
	}
}

func (p *uartSelfTestPeer) waitReply(id string) ([]byte, error) {
	for {
		line, err := p.readLine()
		if err != nil {
			return nil, err
		}
		if protoType(line) == msgReply && protoTopString(line, "id") == id {
			return line, nil
		}
	}
}

func (p *uartSelfTestPeer) waitNeed(id string, next uint32) error {
	for {
		line, err := p.waitType(msgXferNeed, id)
		if err != nil {
			return err
		}
		got, ok := protoTopUint32(line, "next")
		if ok && got == next {
			return nil
		}
		if ok {
			return errors.New("unexpected_xfer_need")
		}
	}
}

func (p *uartSelfTestPeer) readLine() ([]byte, error) {
	p.n = 0
	p.over = false
	for {
		p1, p2 := p.rx.ReadAcquire()
		if len(p1)+len(p2) == 0 {
			select {
			case <-p.ctx.Done():
				return nil, errors.New("transport_closed")
			case <-p.rx.Readable():
				continue
			}
		}
		if idx := findByte(p1, '\n'); idx >= 0 {
			if !p.over && !p.appendLineChunk(p1[:idx]) {
				p.over = true
			}
			p.rx.ReadRelease(idx + 1)
			return p.finishLine()
		}
		if !p.over && !p.appendLineChunk(p1) {
			p.over = true
		}
		if idx := findByte(p2, '\n'); idx >= 0 {
			if !p.over && !p.appendLineChunk(p2[:idx]) {
				p.over = true
			}
			p.rx.ReadRelease(len(p1) + idx + 1)
			return p.finishLine()
		}
		if !p.over && !p.appendLineChunk(p2) {
			p.over = true
		}
		p.rx.ReadRelease(len(p1) + len(p2))
	}
}

func (p *uartSelfTestPeer) appendLineChunk(b []byte) bool {
	if len(b) == 0 {
		return true
	}
	if p.n+len(b) > len(p.lineBuf) {
		p.n = 0
		return false
	}
	copy(p.lineBuf[p.n:], b)
	p.n += len(b)
	return true
}

func (p *uartSelfTestPeer) finishLine() ([]byte, error) {
	if p.over {
		p.n = 0
		p.over = false
		return nil, ErrLineTooLong
	}
	return p.lineBuf[:p.n], nil
}

func (p *uartSelfTestPeer) writeLineBytes(data []byte) error {
	if len(data) > maxLineLen {
		return ErrLineTooLong
	}
	if err := p.writeBytes(data); err != nil {
		return err
	}
	return p.writeByte('\n')
}

func (p *uartSelfTestPeer) writeBytes(data []byte) error {
	written := 0
	for written < len(data) {
		p1, p2 := p.tx.WriteAcquire()
		if len(p1)+len(p2) == 0 {
			select {
			case <-p.ctx.Done():
				return errors.New("transport_closed")
			case <-p.tx.Writable():
				continue
			}
		}
		remaining := data[written:]
		n := copy(p1, remaining)
		remaining = remaining[n:]
		if len(remaining) > 0 && len(p2) > 0 {
			n += copy(p2, remaining)
		}
		p.tx.WriteCommit(n)
		written += n
	}
	return nil
}

func (p *uartSelfTestPeer) writeByte(c byte) error {
	for {
		p1, _ := p.tx.WriteAcquire()
		if len(p1) == 0 {
			select {
			case <-p.ctx.Done():
				return errors.New("transport_closed")
			case <-p.tx.Writable():
				continue
			}
		}
		p1[0] = c
		p.tx.WriteCommit(1)
		return nil
	}
}

func selfTestPayload(n int) []byte {
	out := make([]byte, n)
	var x uint32 = 0x12345678
	for i := range out {
		x = x*1664525 + 1013904223
		out[i] = byte(x >> 24)
	}
	return out
}

func selfTestXXHash(data []byte) string { return xxhashHex(xxhash.Sum32(data, 0)) }

func protoTopBool(line []byte, field string) (bool, bool) {
	i, ok := findTopJSONValue(line, field)
	if !ok {
		return false, false
	}
	if i+4 <= len(line) && string(line[i:i+4]) == "true" {
		return true, true
	}
	if i+5 <= len(line) && string(line[i:i+5]) == "false" {
		return false, true
	}
	return false, false
}
