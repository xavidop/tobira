package export

// WithRewriteSample returns opts with f called during a collection
// export's second-pass rewrite (csv and json), every rewriteSampleEvery
// bytes written. It is for the memory tests of package export_test.
func WithRewriteSample(opts Options, f func()) Options {
	opts.rewriteSample = f
	return opts
}
