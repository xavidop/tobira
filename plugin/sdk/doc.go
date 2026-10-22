// Package sdk turns a driver.Driver into a tobira driver plugin. A plugin
// is a `tobira-driver-<name>` binary whose main calls Serve:
//
//	func main() { sdk.Serve(mydriver.New()) }
//
// Serve speaks the tobira.plugin.v1 protocol (plugin/proto) over
// hashicorp/go-plugin with the handshake in plugin/shared. The host launches
// the binary, checks the magic cookie and protocol version, connects over a
// local socket with AutoMTLS and then calls the driver through gRPC exactly
// as it would call an in-process driver.
//
// What the SDK does for the driver:
//
//   - Conns and Txs live in the plugin behind opaque random handles; an
//     unknown handle is NotFound. Closing a Conn rolls back its open Txs,
//     and every Conn and Tx is released when the plugin shuts down.
//   - Each Conn advertises what driver.CapabilitiesOf reports for it, and
//     a call for a capability the Conn (or Tx) lacks is Unimplemented, so
//     drivers implement only the interfaces they support.
//   - Results stream as frames (columns, batches of up to 256 rows,
//     stats), and every value is checked against the protocol limits.
//   - The host's cancellation reaches the driver through the call's
//     context, mid-stream included.
//   - Errors travel as gRPC statuses carrying the driver error's kind,
//     code, driver and op, with every message redacted with the Conn's
//     ConnConfig (driver.RedactMessage) before it leaves the plugin.
//   - A panic in a driver call becomes an Internal "plugin panic" error for
//     that call only; the panic and its stack go to stderr, redacted with
//     every open Conn's ConnConfig. A panic in a goroutine the driver
//     starts itself cannot be recovered and crashes the plugin.
//   - If the host dies without shutting the plugin down, the plugin notices
//     within a second, releases every Conn and Tx and exits.
//
// What the SDK expects of the driver, beyond the driver package's
// contract: a Watcher's channel must close once the Watch context ends.
// The SDK keeps draining it in the background after the call ends, so a
// channel that never closes leaks that goroutine.
//
// A plugin must never write to stdout: go-plugin owns it for the
// handshake. The ConnConfig.Logger a driver receives writes JSON lines to
// stderr, which the host copies into its debug log, at the level
// TOBIRA_PLUGIN_LOG_LEVEL names (debug, info, warn or error; info when
// unset). A plugin has no
// embedder: the host embeds a text vector query itself and sends the
// vector along with the text.
package sdk
