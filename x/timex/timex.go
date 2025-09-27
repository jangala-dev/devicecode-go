package timex

import "time"

// NowMs returns Unix milliseconds as int64.
func NowMs() int64 { return time.Now().UnixMilli() }

// PeriodFromHz returns a nanosecond period for a requested frequency.
// freqHz==0 is coerced to 1 to avoid division by zero.
func PeriodFromHz(freqHz uint32) uint64 {
	if freqHz == 0 {
		freqHz = 1
	}
	return uint64(1_000_000_000 / uint64(freqHz))
}
