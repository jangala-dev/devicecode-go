package main

import (
	"context"
	"testing"
	"time"

	"devicecode-go/x/shmring"
	"devicecode-go/x/xxhash"
)

func TestReadLinePreservesBytesAfterNewline(t *testing.T) {
	rx := shmring.New(1024)
	p := &peer{rx: rx}
	input := []byte("{\"type\":\"pub\"}\n{\"type\":\"xfer_need\",\"xfer_id\":\"x\",\"next\":0}\n")
	if n := rx.TryWriteFrom(input); n != len(input) {
		t.Fatalf("write = %d, want %d", n, len(input))
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	line, err := p.readLine(ctx)
	if err != nil {
		t.Fatalf("read first line: %v", err)
	}
	if got, want := string(line), "{\"type\":\"pub\"}"; got != want {
		t.Fatalf("first line = %q, want %q", got, want)
	}

	line, err = p.readLine(ctx)
	if err != nil {
		t.Fatalf("read second line: %v", err)
	}
	want := "{\"type\":\"xfer_need\",\"xfer_id\":\"x\",\"next\":0}"
	if got := string(line); got != want {
		t.Fatalf("second line = %q, want %q", got, want)
	}
}

func TestReadLinePreservesBytesAfterNewlineAcrossRingWrap(t *testing.T) {
	rx := shmring.New(64)
	p := &peer{rx: rx}
	pad := []byte("012345678901234567890123456789012345678901234567")
	if n := rx.TryWriteFrom(pad); n != len(pad) {
		t.Fatalf("pad write = %d", n)
	}
	var discard [64]byte
	if n := rx.TryReadInto(discard[:len(pad)]); n != len(pad) {
		t.Fatalf("pad read = %d", n)
	}
	input := []byte("{\"type\":\"a\"}\n{\"type\":\"b\"}\n")
	if n := rx.TryWriteFrom(input); n != len(input) {
		t.Fatalf("write = %d, want %d", n, len(input))
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	line, err := p.readLine(ctx)
	if err != nil {
		t.Fatalf("read first line: %v", err)
	}
	if got, want := string(line), "{\"type\":\"a\"}"; got != want {
		t.Fatalf("first line = %q, want %q", got, want)
	}
	line, err = p.readLine(ctx)
	if err != nil {
		t.Fatalf("read second line: %v", err)
	}
	if got, want := string(line), "{\"type\":\"b\"}"; got != want {
		t.Fatalf("second line = %q, want %q", got, want)
	}
}

func TestPayloadDigestMatchesMaterialisedPayload(t *testing.T) {
	for _, size := range []int{0, 1, 15, 16, 17, 255, 256, 257, 1024, 4097} {
		buf := make([]byte, size)
		for off := 0; off < size; off += chunkSize {
			end := off + chunkSize
			if end > size {
				end = size
			}
			makePayloadChunk(off, buf[off:end])
		}
		got := payloadDigest(size)
		want := hex8(xxhash.Sum32(buf, 0))
		if got != want {
			t.Fatalf("size %d digest = %s, want %s", size, got, want)
		}
	}
}

func TestPayloadChunkMatchesGeneratorOffset(t *testing.T) {
	var buf [17]byte
	chunk := makePayloadChunk(251, buf[:])
	for i, got := range chunk {
		want := payloadByteAt(251 + i)
		if got != want {
			t.Fatalf("byte %d = %d, want %d", i, got, want)
		}
	}
}
