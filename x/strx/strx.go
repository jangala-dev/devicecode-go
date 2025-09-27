package strx

// Coalesce returns s if non-empty, otherwise d.
func Coalesce(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
