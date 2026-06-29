package fabric

// MaxAcceptedChunkSize is the v1 MCU receive-side raw-byte limit for one
// xfer_chunk. The sender chooses the actual chunk size; the MCU only enforces
// this upper bound. The fabric-jsonl/1 release contract requires accepting at
// least 2048 raw bytes per chunk.
const MaxAcceptedChunkSize = 2048

// maxChunkBase64Len is base64.RawURLEncoding.EncodedLen(MaxAcceptedChunkSize).
// Kept as a constant so FabricBuffers can be fully static on TinyGo.
const maxChunkBase64Len = 2731

// FabricBuffers owns all fixed-size scratch storage used by one MCU Fabric
// session. Allocate this once at the top level, or once per session in host
// tests. Transfer-sized buffers must not be constructed in the per-frame or
// per-chunk hot path.
type FabricBuffers struct {
	// RXLines backs the bounded reader queue between the blocking transport
	// reader goroutine and the session reactor. Ownership of each slot is
	// explicit via the free-slot channel in session.run.
	RXLines [lineQueueSize][maxLineLen]byte

	// ChunkRaw receives the decoded raw bytes for one inbound xfer_chunk.
	ChunkRaw [MaxAcceptedChunkSize]byte

	// ChunkB64 is available to future sender-side tests or MCU-originated bulk
	// frames without allocating a per-chunk base64 buffer.
	ChunkB64 [maxChunkBase64Len]byte
}

func NewFabricBuffers() *FabricBuffers { return &FabricBuffers{} }

func ensureFabricBuffers(b *FabricBuffers) *FabricBuffers {
	if b != nil {
		return b
	}
	return NewFabricBuffers()
}

type boundedLineTransport interface {
	ReadLineInto(dst []byte) (int, error)
}
