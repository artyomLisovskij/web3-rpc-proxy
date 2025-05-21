package helpers

// SafeDeref safely dereferences a string pointer
func SafeDeref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}
