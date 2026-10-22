package driver

// SwapDefault replaces Default with a fresh registry for the duration of a
// test and returns a function that restores the previous one. Test-only.
func SwapDefault() (restore func()) {
	old := Default
	Default = NewRegistry()
	return func() { Default = old }
}
