package fabric

import (
	"bytes"
	"encoding/base64"
	"testing"

	"devicecode-go/x/shmring"
)

func TestDecodeChunkDataUsesFixedBuffersAcrossManyChunks(t *testing.T) {
	cfg := DefaultLinkConfig()
	cfg.applyDefaults()
	s := session{cfg: cfg, buffers: NewFabricBuffers()}
	raw := bytes.Repeat([]byte{0x5a}, int(cfg.MaxAcceptedChunkSize))
	encoded := base64.RawURLEncoding.EncodeToString(raw)

	allocs := testing.AllocsPerRun(100, func() {
		for i := 0; i < 64; i++ {
			got, errStr := s.decodeChunkData(encoded)
			if errStr != "" {
				t.Fatalf("decodeChunkData error = %s", errStr)
			}
			if len(got) != len(raw) || got[0] != raw[0] || got[len(got)-1] != raw[len(raw)-1] {
				t.Fatalf("decoded chunk mismatch")
			}
		}
	})
	if allocs > 0 {
		t.Fatalf("decodeChunkData allocations per 64 chunks = %.2f, want zero", allocs)
	}
}

func TestDecodeChunkDataRejectsOversizeBeforeDecode(t *testing.T) {
	cfg := DefaultLinkConfig()
	cfg.applyDefaults()
	s := session{cfg: cfg, buffers: NewFabricBuffers()}
	raw := bytes.Repeat([]byte{0xa5}, int(cfg.MaxAcceptedChunkSize)+1)
	encoded := base64.RawURLEncoding.EncodeToString(raw)

	got, errStr := s.decodeChunkData(encoded)
	if got != nil || errStr != "chunk_too_large" {
		t.Fatalf("decodeChunkData oversize = (%v, %q), want chunk_too_large", got, errStr)
	}
}

func TestShmringTransportReadLineIntoUsesCallerBuffer(t *testing.T) {
	rx := shmringForFabricTest(t, 256)
	tx := shmringForFabricTest(t, 256)
	tr := NewShmringTransportWithBuffers(rx, tx, NewFabricBuffers())
	defer tr.Close()

	line := []byte(`{"type":"ping","sid":"s"}`)
	writeRingForFabricTest(t, rx, append(line, '\n'))

	var dst [maxLineLen]byte
	n, err := tr.ReadLineInto(dst[:])
	if err != nil {
		t.Fatalf("ReadLineInto error = %v", err)
	}
	if string(dst[:n]) != string(line) {
		t.Fatalf("ReadLineInto = %q, want %q", string(dst[:n]), string(line))
	}
}

func shmringForFabricTest(t *testing.T, size int) *shmring.Ring {
	t.Helper()
	return shmring.New(size)
}

func writeRingForFabricTest(t *testing.T, r *shmring.Ring, data []byte) {
	t.Helper()
	written := 0
	for written < len(data) {
		p1, p2 := r.WriteAcquire()
		if len(p1)+len(p2) == 0 {
			t.Fatalf("ring full while writing test data")
		}
		n := copy(p1, data[written:])
		if n < len(data)-written && len(p2) > 0 {
			n += copy(p2, data[written+n:])
		}
		r.WriteCommit(n)
		written += n
	}
}
