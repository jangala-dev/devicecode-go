//go:build !(tinygo && rp2350)

package fabric

import "errors"

var errTransferUnsupported = errors.New("unsupported")

func beginTransfer(meta transferMeta) (transferSink, error) {
	return nil, errTransferUnsupported
}
