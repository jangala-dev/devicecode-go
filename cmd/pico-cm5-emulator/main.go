package main

import (
	"context"
	"encoding/base64"
	"errors"
	"runtime"
	"strconv"
	"time"

	"devicecode-go/bus"
	"devicecode-go/services/hal"
	"devicecode-go/types"
	"devicecode-go/x/shmring"
	"devicecode-go/x/xxhash"
)

const (
	linkUART        = "uart0"
	testTimeout     = 180 * time.Second
	chunkAckTimeout = 750 * time.Millisecond
	maxChunkResends = 32
	cm5SIDPrefix    = "pico-cm5-emulator"
)

var chunkScratch [chunkSize]byte
var lineScratch [4096]byte
var b64Scratch [chunkBase64Max]byte

type peer struct {
	rx        *shmring.Ring
	tx        *shmring.Ring
	n         int
	sid       string
	prepareID string
	jobID     string
	xferID    string
}

func main() {
	ledInit()
	ledOff()
	time.Sleep(3 * time.Second)
	println("0.000 [pico-cm5] bootstrapping bus + HAL")

	ctx := context.Background()
	b := bus.NewBus(4, "+", "#")
	halConn := b.NewConnection("hal")
	ctlConn := b.NewConnection("pico-cm5")
	go hal.Run(ctx, halConn)

	time.Sleep(250 * time.Millisecond)
	opened, err := openSerial(ctx, ctlConn, linkUART, 512, 512)
	if err != nil {
		fail("serial open failed", err)
	}
	rx := shmring.Get(shmring.Handle(opened.RXHandle))
	tx := shmring.Get(shmring.Handle(opened.TXHandle))
	if rx == nil || tx == nil {
		fail("serial ring resolution failed", errors.New("nil_ring"))
	}
	println("0.000 [pico-cm5] uart0 opened; starting Fabric CM5-emulator script")

	ledOn()
	runID := makeRunID()
	p := &peer{
		rx:        rx,
		tx:        tx,
		sid:       cm5SIDPrefix + "-sid-" + runID,
		prepareID: "pico-cm5-prepare-" + runID,
		jobID:     "pico-cm5-job-" + runID,
		xferID:    "pico-cm5-xfer-" + runID,
	}
	println("0.000 [pico-cm5] session sid=", p.sid, "xfer=", p.xferID)
	if err := p.run(ctx); err != nil {
		fail("fabric script failed", err)
	}
	println("0.000 [pico-cm5] PASS: Fabric prepare + transfer completed")
	for {
		ledOn()
		printMem()
		time.Sleep(1800 * time.Millisecond)
		ledOff()
		time.Sleep(200 * time.Millisecond)
	}
}

func fail(msg string, err error) {
	println("0.000 [pico-cm5] FAIL:", msg, err.Error())
	for {
		ledOn()
		time.Sleep(120 * time.Millisecond)
		ledOff()
		time.Sleep(120 * time.Millisecond)
	}
}

func (p *peer) run(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, testTimeout)
	defer cancel()

	digest := payloadDigest(payloadSize)
	println("0.000 [pico-cm5] payload bytes=", payloadSize, "chunk=", chunkSize, "digest=", digest)

	if err := p.writeHello(ctx); err != nil {
		return err
	}
	println("0.000 [pico-cm5] hello sent")
	if _, err := p.waitType(ctx, "hello_ack", ""); err != nil {
		return err
	}
	println("0.000 [pico-cm5] hello_ack received")

	if err := p.writePrepare(ctx, p.prepareID); err != nil {
		return err
	}
	println("0.000 [pico-cm5] prepare-update sent")
	if err := p.waitReplyOK(ctx, p.prepareID); err != nil {
		return err
	}
	println("0.000 [pico-cm5] prepare-update ok")

	cm5TraceEvent("phase_xfer_begin_write")
	if err := p.writeXferBegin(ctx, p.xferID, uint32(payloadSize), digest); err != nil {
		return err
	}
	println("0.000 [pico-cm5] xfer_begin sent")
	cm5TraceEvent("phase_wait_xfer_ready")
	if _, err := p.waitType(ctx, "xfer_ready", p.xferID); err != nil {
		return err
	}
	println("0.000 [pico-cm5] xfer_ready received")
	cm5TraceEvent("phase_wait_xfer_need_0")
	if err := p.transferPayload(ctx, p.xferID); err != nil {
		return err
	}
	cm5TraceEvent("phase_commit_write")
	if err := p.writeXferCommit(ctx, p.xferID, uint32(payloadSize), digest); err != nil {
		return err
	}
	if _, err := p.waitType(ctx, "xfer_done", p.xferID); err != nil {
		return err
	}
	println("0.000 [pico-cm5] xfer_done")
	return nil
}

func (p *peer) transferPayload(ctx context.Context, id string) error {
	maxSentEnd := uint32(0)
	lastAck := uint32(0)
	lastSentOff := uint32(0)
	haveOutstanding := false
	resendsWithoutProgress := 0

	for {
		need, err := p.waitNeedWithTimeout(ctx, id, chunkAckTimeout)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) && haveOutstanding {
				resendsWithoutProgress++
				println("0.000 [pico-cm5] ack timeout resend offset=", int(lastSentOff), "retry=", resendsWithoutProgress)
				if resendsWithoutProgress > maxChunkResends {
					return errors.New("too_many_ack_timeouts_at_offset:" + strconv.FormatUint(uint64(lastSentOff), 10))
				}
				if err := p.sendPayloadChunk(ctx, id, lastSentOff); err != nil {
					return err
				}
				continue
			}
			return err
		}
		if need > uint32(payloadSize) {
			return errors.New("bad_xfer_need_next_too_large:" + strconv.FormatUint(uint64(need), 10))
		}
		if need == uint32(payloadSize) {
			println("0.000 [pico-cm5] chunk ack next=", int(need))
			return nil
		}
		if need > maxSentEnd {
			return errors.New("bad_xfer_need_future:" + strconv.FormatUint(uint64(need), 10) + ":max_sent=" + strconv.FormatUint(uint64(maxSentEnd), 10))
		}

		if need > lastAck {
			lastAck = need
			resendsWithoutProgress = 0
			haveOutstanding = false
			if shouldPrintAck(need) {
				println("0.000 [pico-cm5] chunk ack next=", int(need))
			}
		} else if need < maxSentEnd {
			resendsWithoutProgress++
			println("0.000 [pico-cm5] retry need next=", int(need), "max_sent=", int(maxSentEnd), "retry=", resendsWithoutProgress)
			if resendsWithoutProgress > maxChunkResends {
				return errors.New("too_many_retries_at_older_offset:" + strconv.FormatUint(uint64(need), 10))
			}
		}

		if err := p.sendPayloadChunk(ctx, id, need); err != nil {
			return err
		}
		lastSentOff = need
		haveOutstanding = true
		end := need + uint32(chunkSize)
		if end > uint32(payloadSize) {
			end = uint32(payloadSize)
		}
		if end > maxSentEnd {
			maxSentEnd = end
		}
	}
}

func (p *peer) sendPayloadChunk(ctx context.Context, id string, off uint32) error {
	end := int(off) + chunkSize
	if end > payloadSize {
		end = payloadSize
	}
	chunk := makePayloadChunk(int(off), chunkScratch[:end-int(off)])
	cm5TraceEventKV("phase_chunk_write", "offset", strconv.Itoa(int(off)))
	return p.writeXferChunk(ctx, id, off, chunk)
}

func shouldPrintAck(next uint32) bool {
	if payloadSize <= 4096 {
		return true
	}
	return next != 0 && (next%4096 == 0 || next == uint32(payloadSize))
}

func makeRunID() string {
	// The SID is a session identifier, not a stable node identity. Make it
	// change across emulator resets so the MCU can distinguish a fresh CM5
	// session from a duplicate hello in an existing session, and abort any
	// old in-flight transfer state accordingly.
	n := uint64(time.Now().UnixNano())
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	n ^= uint64(ms.Mallocs) << 17
	n ^= uint64(ms.Alloc) << 1
	if n == 0 {
		n = 1
	}
	return strconv.FormatUint(n, 16)
}

func openSerial(ctx context.Context, conn *bus.Connection, name string, rxSize, txSize int) (types.SerialSessionOpened, error) {
	evT := bus.T("hal", "cap", "io", "serial", name, "event", "session_opened")
	sub := conn.Subscribe(evT)
	defer conn.Unsubscribe(sub)

	ctrlT := bus.T("hal", "cap", "io", "serial", name, "control", "session_open")
	reqCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if _, err := conn.RequestWait(reqCtx, conn.NewMessage(ctrlT, types.SerialSessionOpen{RXSize: rxSize, TXSize: txSize}, false)); err != nil {
		return types.SerialSessionOpened{}, err
	}
	for {
		select {
		case m := <-sub.Channel():
			if rep, ok := m.Payload.(types.SerialSessionOpened); ok {
				return rep, nil
			}
		case <-reqCtx.Done():
			return types.SerialSessionOpened{}, reqCtx.Err()
		}
	}
}

func (p *peer) writeHello(ctx context.Context) error {
	b := make([]byte, 0, 96)
	b = append(b, `{"type":"hello","proto":"fabric-jsonl/1","sid":"`...)
	b = appendJSONString(b, p.sid)
	b = append(b, `","node":"bigbox-cm5"}`...)
	return p.writeLine(ctx, b)
}

func (p *peer) writePrepare(ctx context.Context, id string) error {
	b := make([]byte, 0, 192)
	b = append(b, `{"type":"call","id":"`...)
	b = appendJSONString(b, id)
	b = append(b, `","topic":["cap","self","updater","main","rpc","prepare-update"],"payload":{"job_id":"`...)
	b = appendJSONString(b, p.jobID)
	b = append(b, `","target":"mcu","expected_image_id":"pico-cm5-hwtest-image"}}`...)
	return p.writeLine(ctx, b)
}

func (p *peer) writeXferBegin(ctx context.Context, id string, size uint32, digest string) error {
	b := make([]byte, 0, 192)
	b = append(b, `{"type":"xfer_begin","xfer_id":"`...)
	b = appendJSONString(b, id)
	b = append(b, `","target":"updater/main","size":`...)
	b = strconv.AppendUint(b, uint64(size), 10)
	b = append(b, `,"digest_alg":"xxhash32","digest":"`...)
	b = appendJSONString(b, digest)
	b = append(b, `","meta":{"source":"pico-cm5-emulator"}}`...)
	return p.writeLine(ctx, b)
}

func (p *peer) writeXferChunk(ctx context.Context, id string, off uint32, chunk []byte) error {
	n := base64.RawURLEncoding.EncodedLen(len(chunk))
	if n > len(b64Scratch) {
		return errors.New("chunk_too_large")
	}
	base64.RawURLEncoding.Encode(b64Scratch[:n], chunk)
	chunkDigest := hex8(xxhash.Sum32(chunk, 0))
	b := make([]byte, 0, 160+n)
	b = append(b, `{"type":"xfer_chunk","xfer_id":"`...)
	b = appendJSONString(b, id)
	b = append(b, `","offset":`...)
	b = strconv.AppendUint(b, uint64(off), 10)
	b = append(b, `,"data":"`...)
	b = append(b, b64Scratch[:n]...)
	b = append(b, `","chunk_digest":"`...)
	b = appendJSONString(b, chunkDigest)
	b = append(b, `"}`...)
	return p.writeLine(ctx, b)
}

func (p *peer) writeXferCommit(ctx context.Context, id string, size uint32, digest string) error {
	b := make([]byte, 0, 144)
	b = append(b, `{"type":"xfer_commit","xfer_id":"`...)
	b = appendJSONString(b, id)
	b = append(b, `","size":`...)
	b = strconv.AppendUint(b, uint64(size), 10)
	b = append(b, `,"digest_alg":"xxhash32","digest":"`...)
	b = appendJSONString(b, digest)
	b = append(b, `"}`...)
	return p.writeLine(ctx, b)
}

func (p *peer) writePong(ctx context.Context, sid string) error {
	b := make([]byte, 0, 64)
	b = append(b, `{"type":"pong","sid":"`...)
	b = appendJSONString(b, sid)
	b = append(b, `"}`...)
	return p.writeLine(ctx, b)
}

func (p *peer) waitReplyOK(ctx context.Context, id string) error {
	for {
		line, err := p.readLine(ctx)
		if err != nil {
			return err
		}
		t := topString(line, "type")
		switch t {
		case "reply":
			if topString(line, "id") != id {
				continue
			}
			ok, seen := topBool(line, "ok")
			if seen && ok {
				return nil
			}
			return errors.New("reply_error:" + topString(line, "err"))
		case "ping":
			_ = p.writePong(ctx, topString(line, "sid"))
		case "pub":
			logPub(line)
		default:
			if t != "" {
				println("0.000 [pico-cm5] rx", t)
			}
		}
	}
}

func (p *peer) waitType(ctx context.Context, wantType, wantXfer string) ([]byte, error) {
	for {
		line, err := p.readLine(ctx)
		if err != nil {
			return nil, err
		}
		t := topString(line, "type")
		switch t {
		case wantType:
			if wantXfer == "" || topString(line, "xfer_id") == wantXfer {
				return line, nil
			}
		case "xfer_abort":
			if wantXfer == "" || topString(line, "xfer_id") == wantXfer {
				return nil, errors.New("xfer_abort:" + topString(line, "err"))
			}
		case "reply":
			if errText := topString(line, "err"); errText != "" {
				println("0.000 [pico-cm5] stray reply err=", errText)
			}
		case "ping":
			_ = p.writePong(ctx, topString(line, "sid"))
		case "pub":
			logPub(line)
		default:
			if t != "" {
				println("0.000 [pico-cm5] rx", t)
			}
		}
	}
}

func (p *peer) waitNeed(ctx context.Context, id string) (uint32, error) {
	for {
		line, err := p.waitType(ctx, "xfer_need", id)
		if err != nil {
			return 0, err
		}
		got, ok := topUint(line, "next")
		if ok {
			return got, nil
		}
		return 0, errors.New("bad_xfer_need_missing_next:" + cm5TracePreview(line))
	}
}

func (p *peer) waitNeedWithTimeout(ctx context.Context, id string, d time.Duration) (uint32, error) {
	waitCtx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	return p.waitNeed(waitCtx, id)
}

func (p *peer) writeLine(ctx context.Context, b []byte) error {
	if len(b) == 0 || b[len(b)-1] != '\n' {
		b = append(b, '\n')
	}
	cm5TraceFrame("tx", b)
	off := 0
	for off < len(b) {
		if n := p.tx.TryWriteFrom(b[off:]); n > 0 {
			off += n
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-p.tx.Writable():
		}
	}
	return nil
}

func (p *peer) readLine(ctx context.Context) ([]byte, error) {
	for {
		span, _ := p.rx.ReadAcquire()
		if len(span) > 0 {
			line, consumed, ok, err := p.consumeLineSpan(span)
			if consumed > 0 {
				p.rx.ReadRelease(consumed)
			}
			if err != nil {
				return nil, err
			}
			if ok {
				return line, nil
			}
			continue
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-p.rx.Readable():
		}
	}
}

func (p *peer) consumeLineSpan(span []byte) (line []byte, consumed int, ok bool, err error) {
	for i, c := range span {
		consumed = i + 1
		if c == '\r' {
			continue
		}
		if c == '\n' {
			if p.n == 0 {
				continue
			}
			line := lineScratch[:p.n]
			cm5TraceFrame("rx", line)
			p.n = 0
			return line, consumed, true, nil
		}
		if p.n >= len(lineScratch) {
			p.n = 0
			return nil, consumed, false, errors.New("line_too_long")
		}
		lineScratch[p.n] = c
		p.n++
	}
	return nil, consumed, false, nil
}

func payloadByteAt(i int) byte {
	return byte((i*37 + 11) & 0xff)
}

func makePayloadChunk(off int, dst []byte) []byte {
	for i := range dst {
		dst[i] = payloadByteAt(off + i)
	}
	return dst
}

func payloadDigest(size int) string {
	h := xxhash.New(0)
	var buf [chunkSize]byte
	for off := 0; off < size; off += chunkSize {
		end := off + chunkSize
		if end > size {
			end = size
		}
		_, _ = h.Write(makePayloadChunk(off, buf[:end-off]))
	}
	return hex8(h.Sum32())
}

func hex8(v uint32) string {
	const h = "0123456789abcdef"
	var b [8]byte
	for i := 7; i >= 0; i-- {
		b[i] = h[v&0xf]
		v >>= 4
	}
	return string(b[:])
}

func appendJSONString(b []byte, s string) []byte {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\\', '"':
			b = append(b, '\\', c)
		case '\n':
			b = append(b, '\\', 'n')
		case '\r':
			b = append(b, '\\', 'r')
		case '\t':
			b = append(b, '\\', 't')
		default:
			b = append(b, c)
		}
	}
	return b
}

func findKey(line []byte, key string) int {
	// Locate a top-level-ish JSON field by string pattern. The Fabric frames used
	// by this emulator are compact and do not contain the same field names inside
	// escaped strings, which is sufficient for this smoke firmware.
	patLen := len(key) + 2
	for i := 0; i+patLen < len(line); i++ {
		if line[i] != '"' {
			continue
		}
		if string(line[i+1:i+1+len(key)]) != key || line[i+1+len(key)] != '"' {
			continue
		}
		j := i + patLen
		for j < len(line) && (line[j] == ' ' || line[j] == '\t') {
			j++
		}
		if j < len(line) && line[j] == ':' {
			j++
			for j < len(line) && (line[j] == ' ' || line[j] == '\t') {
				j++
			}
			return j
		}
	}
	return -1
}

func topString(line []byte, key string) string {
	i := findKey(line, key)
	if i < 0 || i >= len(line) || line[i] != '"' {
		return ""
	}
	i++
	start := i
	for i < len(line) {
		if line[i] == '\\' {
			i += 2
			continue
		}
		if line[i] == '"' {
			return string(line[start:i])
		}
		i++
	}
	return ""
}

func topBool(line []byte, key string) (bool, bool) {
	i := findKey(line, key)
	if i < 0 || i >= len(line) {
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

func topUint(line []byte, key string) (uint32, bool) {
	i := findKey(line, key)
	if i < 0 || i >= len(line) || line[i] < '0' || line[i] > '9' {
		return 0, false
	}
	var v uint32
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		v = v*10 + uint32(line[i]-'0')
		i++
	}
	return v, true
}

func logPub(line []byte) {
	// Keep pub logging light. Detailed frame logs are intentionally omitted so the
	// emulator remains closer to the 3 KB stack target.
	if topic := topString(line, "topic"); topic != "" {
		println("0.000 [pico-cm5] pub", topic)
	}
}

func cm5TraceFrame(dir string, b []byte) {
	if !picoCM5TraceEnabled {
		return
	}
	line := b
	if len(line) > 0 && line[len(line)-1] == '\n' {
		line = line[:len(line)-1]
	}
	println(
		"0.000 [pico-cm5-trace]", dir,
		"type", topString(line, "type"),
		"xfer", topString(line, "xfer_id"),
		"id", topString(line, "id"),
		"next", traceUint(line, "next"),
		"len", len(line),
		"line", cm5TracePreview(line),
	)
}

func cm5TraceEvent(event string) {
	if !picoCM5TraceEnabled {
		return
	}
	println("0.000 [pico-cm5-trace]", event)
}

func cm5TraceEventKV(event, key, value string) {
	if !picoCM5TraceEnabled {
		return
	}
	println("0.000 [pico-cm5-trace]", event, key, value)
}

func cm5TracePreview(data []byte) string {
	const max = 220
	if len(data) > max {
		data = data[:max]
	}
	out := make([]byte, 0, len(data)*2+3)
	for _, c := range data {
		switch c {
		case '\n':
			out = append(out, '\\', 'n')
		case '\r':
			out = append(out, '\\', 'r')
		case '\t':
			out = append(out, '\\', 't')
		default:
			if c < 0x20 || c > 0x7e {
				out = append(out, '\\', 'x', hexNibble(c>>4), hexNibble(c))
			} else {
				out = append(out, c)
			}
		}
	}
	if len(data) == max {
		out = append(out, '.', '.', '.')
	}
	return string(out)
}

func traceUint(line []byte, key string) uint32 {
	v, _ := topUint(line, key)
	return v
}

func hexNibble(v byte) byte {
	v &= 0x0f
	if v < 10 {
		return '0' + v
	}
	return 'a' + (v - 10)
}

func printMem() {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	println("0.000 [pico-cm5] mem alloc:", int(m.Alloc), "heapSys:", int(m.HeapSys), "mallocs:", int(m.Mallocs), "frees:", int(m.Frees))
}
