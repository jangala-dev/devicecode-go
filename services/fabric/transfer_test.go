package fabric

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"devicecode-go/bus"
)

type fakeTransferFactory struct {
	beginMeta transferMeta
	beginErr  error
	sink      *fakeTransferSink
}

func (f *fakeTransferFactory) Begin(meta transferMeta) (transferSink, error) {
	f.beginMeta = meta
	if f.beginErr != nil {
		return nil, f.beginErr
	}
	if f.sink == nil {
		f.sink = &fakeTransferSink{}
	}
	return f.sink, nil
}

type fakeTransferSink struct {
	seqs         []uint32
	offs         []uint32
	writes       [][]byte
	writeErr     error
	commitErr    error
	applyErr     error
	commitInfo   transferInfo
	committed    bool
	applied      bool
	abortReasons []string
}

func (s *fakeTransferSink) WriteChunk(seq, off uint32, data []byte) error {
	if s.writeErr != nil {
		return s.writeErr
	}
	s.seqs = append(s.seqs, seq)
	s.offs = append(s.offs, off)
	s.writes = append(s.writes, append([]byte(nil), data...))
	return nil
}

func (s *fakeTransferSink) Commit() (transferInfo, error) {
	if s.commitErr != nil {
		return transferInfo{}, s.commitErr
	}
	s.committed = true
	return s.commitInfo, nil
}

func (s *fakeTransferSink) Apply() error {
	s.applied = true
	return s.applyErr
}

func (s *fakeTransferSink) Abort(reason string) error {
	s.abortReasons = append(s.abortReasons, reason)
	return nil
}

func runSessionWithFactory(ctx context.Context, tr Transport, conn *bus.Connection, factory transferFactory) {
	s := session{
		linkID:          defaultLinkID,
		nodeID:          "mcu-1",
		peerID:          "cm5-local",
		localSID:        "mcu-sid-test",
		tr:              tr,
		conn:            conn,
		transferFactory: factory,
	}
	s.run(ctx)
}

func rawURL(data []byte) string {
	return base64.RawURLEncoding.EncodeToString(data)
}

func sha256String(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestTransferBeginUnsupportedOnHost(t *testing.T) {
	b := newBus()
	cm5, mcu := pipePair()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, mcu, b.NewConnection("fabric"), "mcu-1", "cm5-local")

	bringUp(t, cm5)

	payload := []byte("firmware")
	sendMsg(t, cm5, protoXferBegin{
		T:        msgXferBegin,
		ID:       "xfer-1",
		Kind:     "firmware.rp2350",
		Name:     "fw.bin",
		Format:   "bin",
		Enc:      "b64url",
		Size:     uint32(len(payload)),
		ChunkRaw: 4,
		Chunks:   2,
		SHA256:   sha256String(payload),
	})

	ready := readMsg[protoXferReady](t, cm5)
	if ready.T != msgXferReady || ready.ID != "xfer-1" || ready.OK {
		t.Fatalf("bad xfer_ready: %+v", ready)
	}
	if ready.Err != "unsupported" {
		t.Fatalf("xfer_ready.Err = %q, want unsupported", ready.Err)
	}
}

func TestTransferReceiveSuccess(t *testing.T) {
	b := newBus()
	cm5, mcu := pipePair()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	factory := &fakeTransferFactory{
		sink: &fakeTransferSink{
			commitInfo: transferInfo{
				BytesWritten: 10,
				SlotXIPAddr:  0x10280000,
			},
		},
	}

	go runSessionWithFactory(ctx, mcu, b.NewConnection("fabric"), factory)
	bringUp(t, cm5)

	payload := []byte("abcdefghij")
	sendMsg(t, cm5, protoXferBegin{
		T:        msgXferBegin,
		ID:       "xfer-2",
		Kind:     "firmware.rp2350",
		Name:     "fw.bin",
		Format:   "bin",
		Enc:      "b64url",
		Size:     uint32(len(payload)),
		ChunkRaw: 4,
		Chunks:   3,
		SHA256:   sha256String(payload),
	})

	ready := readMsg[protoXferReady](t, cm5)
	if !ready.OK || ready.Next == nil || *ready.Next != 0 {
		t.Fatalf("bad xfer_ready: %+v", ready)
	}

	parts := [][]byte{
		payload[:4],
		payload[4:8],
		payload[8:],
	}
	off := uint32(0)
	for i, part := range parts {
		sendMsg(t, cm5, protoXferChunk{
			T:     msgXferChunk,
			ID:    "xfer-2",
			Seq:   uint32(i),
			Off:   off,
			N:     uint32(len(part)),
			CRC32: crc32Hex(part),
			Data:  rawURL(part),
		})
		need := readMsg[protoXferNeed](t, cm5)
		if need.Next != uint32(i+1) || need.Err != "" {
			t.Fatalf("bad xfer_need[%d]: %+v", i, need)
		}
		off += uint32(len(part))
	}

	sendMsg(t, cm5, protoXferCommit{
		T:      msgXferCommit,
		ID:     "xfer-2",
		Size:   uint32(len(payload)),
		SHA256: sha256String(payload),
	})

	done := readMsg[protoXferDone](t, cm5)
	if !done.OK || done.ID != "xfer-2" {
		t.Fatalf("bad xfer_done: %+v", done)
	}
	var info transferInfo
	if err := json.Unmarshal(done.Info, &info); err != nil {
		t.Fatalf("unmarshal info: %v", err)
	}
	if info.BytesWritten != 10 || info.SlotXIPAddr != 0x10280000 {
		t.Fatalf("bad transfer info: %+v", info)
	}

	time.Sleep(20 * time.Millisecond)

	if got := string(factory.sink.writes[0]) + string(factory.sink.writes[1]) + string(factory.sink.writes[2]); got != string(payload) {
		t.Fatalf("sink writes = %q, want %q", got, payload)
	}
	if !factory.sink.committed {
		t.Fatal("sink.Commit was not called")
	}
	if !factory.sink.applied {
		t.Fatal("sink.Apply was not called")
	}
}

func TestTransferChunkBadCRCRequestsResend(t *testing.T) {
	b := newBus()
	cm5, mcu := pipePair()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	factory := &fakeTransferFactory{sink: &fakeTransferSink{}}
	go runSessionWithFactory(ctx, mcu, b.NewConnection("fabric"), factory)
	bringUp(t, cm5)

	payload := []byte("abcd")
	sendMsg(t, cm5, protoXferBegin{
		T:        msgXferBegin,
		ID:       "xfer-3",
		Kind:     "firmware.rp2350",
		Name:     "fw.bin",
		Format:   "bin",
		Enc:      "b64url",
		Size:     uint32(len(payload)),
		ChunkRaw: 4,
		Chunks:   1,
		SHA256:   sha256String(payload),
	})
	_ = readMsg[protoXferReady](t, cm5)

	sendMsg(t, cm5, protoXferChunk{
		T:     msgXferChunk,
		ID:    "xfer-3",
		Seq:   0,
		Off:   0,
		N:     uint32(len(payload)),
		CRC32: "deadbeef",
		Data:  rawURL(payload),
	})

	need := readMsg[protoXferNeed](t, cm5)
	if need.Next != 0 || need.Err != "bad_crc" {
		t.Fatalf("bad xfer_need: %+v", need)
	}
	if len(factory.sink.writes) != 0 {
		t.Fatalf("sink received %d writes, want 0", len(factory.sink.writes))
	}
}

func TestTransferCommitHashMismatchReturnsDoneError(t *testing.T) {
	b := newBus()
	cm5, mcu := pipePair()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	factory := &fakeTransferFactory{sink: &fakeTransferSink{}}
	go runSessionWithFactory(ctx, mcu, b.NewConnection("fabric"), factory)
	bringUp(t, cm5)

	payload := []byte("abcd")
	sendMsg(t, cm5, protoXferBegin{
		T:        msgXferBegin,
		ID:       "xfer-4",
		Kind:     "firmware.rp2350",
		Name:     "fw.bin",
		Format:   "bin",
		Enc:      "b64url",
		Size:     uint32(len(payload)),
		ChunkRaw: 4,
		Chunks:   1,
		SHA256:   sha256String(payload),
	})
	_ = readMsg[protoXferReady](t, cm5)

	sendMsg(t, cm5, protoXferChunk{
		T:     msgXferChunk,
		ID:    "xfer-4",
		Seq:   0,
		Off:   0,
		N:     uint32(len(payload)),
		CRC32: crc32Hex(payload),
		Data:  rawURL(payload),
	})
	_ = readMsg[protoXferNeed](t, cm5)

	sendMsg(t, cm5, protoXferCommit{
		T:      msgXferCommit,
		ID:     "xfer-4",
		Size:   uint32(len(payload)),
		SHA256: strings.Repeat("0", 64),
	})

	done := readMsg[protoXferDone](t, cm5)
	if done.OK || done.Err != "sha256_mismatch" {
		t.Fatalf("bad xfer_done: %+v", done)
	}
	if len(factory.sink.abortReasons) == 0 {
		t.Fatal("expected sink abort on hash mismatch")
	}
}
