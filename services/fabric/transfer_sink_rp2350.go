//go:build tinygo && rp2350 && !flash_unsafe

// Default RP2350 transfer sink for the fabric-protocol baseline. Rejects all
// transfers at xfer_begin: signed-image verification and staged flash writes
// land in fabric-update via the receiver topic
// `raw/member/mcu/cap/updater/main/rpc/receive` and `pico2-a-b/imagev1/`. Until
// that path lands, the safe default is to refuse incoming transfers rather
// than flash unverified bytes directly into the inactive slot.
//
// To re-enable direct (unverified) abupdate flashing for development or
// hardware bring-up, build with `-tags=flash_unsafe`. See
// transfer_sink_rp2350_unsafe.go.

package fabric

import "errors"

var errTransferUnsupported = errors.New("staging_unavailable: signed-image receiver not present in this build")

func beginTransfer(meta transferMeta) (transferSink, error) {
	_ = meta
	return nil, errTransferUnsupported
}
