// registry.go
package shmring

import "sync"

// Handle is an opaque identifier for a registered Ring.
// The zero handle is invalid.
type Handle uint32

var (
	regMu   sync.RWMutex
	reg            = map[Handle]*Ring{}
	nextHdl Handle = 1
)

// NewRegistered allocates a new Ring of the given power-of-two size (>= 2),
// registers it, and returns the Handle and *Ring.
// The underlying ring is identical to one created with New(size).
func NewRegistered(size int) (Handle, *Ring) {
	r := New(size)
	regMu.Lock()
	h := nextHdl
	nextHdl++
	reg[h] = r
	regMu.Unlock()
	return h, r
}

// Register adds an existing Ring to the registry and returns a new Handle.
func Register(r *Ring) Handle {
	if r == nil {
		return 0
	}
	regMu.Lock()
	h := nextHdl
	nextHdl++
	reg[h] = r
	regMu.Unlock()
	return h
}

// Get returns the *Ring for a Handle, or nil if the handle is zero or unknown.
func Get(h Handle) *Ring {
	if h == 0 {
		return nil
	}
	regMu.RLock()
	r := reg[h]
	regMu.RUnlock()
	return r
}

// Close removes a Handle from the registry. It does not modify or close
// the underlying Ring; any existing pointers remain valid.
func Close(h Handle) {
	regMu.Lock()
	delete(reg, h)
	regMu.Unlock()
}
