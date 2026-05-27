package fabric

import "devicecode-go/x/xxhash"

func traceLine(dir string, data []byte) {
	if !fabricTraceEnabled {
		return
	}
	println("[fabric-trace]", dir, "len", len(data), "line", tracePreview(data))
}

func tracePreview(data []byte) string {
	const max = 200
	if len(data) > max {
		data = data[:max]
	}
	out := make([]byte, 0, len(data)*2+3)
	for _, b := range data {
		switch b {
		case '\n':
			out = append(out, '\\', 'n')
		case '\r':
			out = append(out, '\\', 'r')
		case '\t':
			out = append(out, '\\', 't')
		default:
			if b < 0x20 || b > 0x7e {
				out = append(out, '\\', 'x')
				out = append(out, hexNibble(b>>4), hexNibble(b))
			} else {
				out = append(out, b)
			}
		}
	}
	if len(data) == max {
		out = append(out, '.', '.', '.')
	}
	return string(out)
}

func traceTail(data []byte) string {
	const max = 200
	if len(data) > max {
		data = data[len(data)-max:]
	}
	return tracePreview(data)
}

func traceHash(data []byte) string {
	return xxhashHex(xxhash.Sum32(data, 0))
}

func hexNibble(v byte) byte {
	v &= 0x0f
	if v < 10 {
		return '0' + v
	}
	return 'a' + (v - 10)
}
