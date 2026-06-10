//go:build fabric_xfer_probe

package fabric

import "strconv"

const xferProbeEnabled = true

var xferProbeLastProgress uint32

func xferProbe(args ...any) {
	if !xferProbeShouldPrint(args...) {
		return
	}
	print("[fabric-xfer-probe]")
	for _, a := range args {
		print(" ")
		switch v := a.(type) {
		case string:
			print(v)
		case int:
			print(strconv.Itoa(v))
		case uint32:
			print(strconv.FormatUint(uint64(v), 10))
		case uint64:
			print(strconv.FormatUint(v, 10))
		case bool:
			if v {
				print("true")
			} else {
				print("false")
			}
		case error:
			if v != nil {
				print(v.Error())
			}
		default:
			print("?")
		}
	}
	println()
}

func xferProbeShouldPrint(args ...any) bool {
	if len(args) == 0 {
		return false
	}
	event, _ := args[0].(string)
	switch event {
	case "chunk_rx", "write_start", "write_done":
		return false
	case "need_after_write":
		// Progress only. Per-chunk printing materially perturbs UART RX service
		// while the peer is already sending the next chunk.
		next, ok := xferProbeArgUint32(args, "next")
		if !ok {
			return false
		}
		if next == 0 || next-xferProbeLastProgress >= 32768 {
			xferProbeLastProgress = next
			return true
		}
		return false
	default:
		return true
	}
}

func xferProbeArgUint32(args []any, key string) (uint32, bool) {
	for i := 0; i+1 < len(args); i++ {
		k, ok := args[i].(string)
		if !ok || k != key {
			continue
		}
		switch v := args[i+1].(type) {
		case uint32:
			return v, true
		case int:
			if v >= 0 {
				return uint32(v), true
			}
		case uint64:
			return uint32(v), true
		}
	}
	return 0, false
}
