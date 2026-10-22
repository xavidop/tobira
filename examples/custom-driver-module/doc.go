// Package memdriver is a tiny in-memory tobira driver, written to be read.
//
// It serves one namespace ("main") with one collection ("tobira_people"),
// copied from the conformance kit's People dataset when a connection opens.
// The only query it understands is:
//
//	select * from <collection>
//
// It has no optional capabilities, so it implements only driver.Conn. The test
// in this module runs drivertest.Run against it in Strict mode, which is the
// bar every tobira driver has to clear.
//
// See https://xavidop.github.io/tobira/docs/write-a-driver/ for the walkthrough.
package memdriver
