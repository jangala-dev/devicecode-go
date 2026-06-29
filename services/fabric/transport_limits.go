package fabric

import "fmt"

// maxLineLen caps a single fabric frame (line-delimited JSON) end-to-end.
// It must clear the release transfer chunk: 1024 raw bytes becomes about
// 1366 base64url chars, plus JSON envelope and newline. 4096 leaves margin
// while keeping malformed lines bounded.
const maxLineLen = 4096

var ErrLineTooLong = fmt.Errorf("line exceeds %d bytes", maxLineLen)
