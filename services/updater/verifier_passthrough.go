package updater

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
)

// passthroughVerifier accepts any artefact, streams its bytes straight
// into sink while computing SHA-256, and returns a synthetic manifest
// with the artefact length + computed hash. Intended for the bringup
// stack on this branch where the signed-image v1 envelope (header +
// canonical manifest + Ed25519 signature) is not yet implemented.
//
// Replace with a real verifier when fabric-security lands; this exists
// so fw-update-e2e can drive the staging → applier → reboot path
// end-to-end without the signed-image scaffolding in place.
type passthroughVerifier struct {
	identity Identity
}

// PassthroughVerifier returns a Verifier that accepts any artefact and
// fills the manifest with identity (caller-supplied), the artefact
// length, and the SHA-256 of the streamed payload. Reboot-time apply
// is gated by the Applier; a passthrough verifier without a real
// applier still ends with state=failed(apply_unavailable) at commit.
func PassthroughVerifier(identity Identity) Verifier {
	return passthroughVerifier{identity: identity}
}

func (v passthroughVerifier) Verify(r io.Reader, sink SlotSink) (Manifest, error) {
	if sink == nil {
		return Manifest{}, errors.New("passthrough_verifier: nil sink")
	}
	hasher := sha256.New()
	mw := io.MultiWriter(sink, hasher)
	n, err := io.Copy(mw, r)
	if err != nil {
		_ = sink.Abort()
		return Manifest{}, err
	}
	return Manifest{
		Version:       v.identity.Version,
		BuildID:       v.identity.Build,
		ImageID:       v.identity.ImageID,
		PayloadSHA256: hex.EncodeToString(hasher.Sum(nil)),
		PayloadLength: uint32(n),
	}, nil
}
