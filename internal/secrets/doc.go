// Package secrets turns a config.Connection into a driver.ConnConfig with its
// password resolved.
//
// The password comes from, in order: the TOBIRA_CONN_<NAME>_PASSWORD
// environment variable, the connection's password: mamori ref, or the
// password embedded in the DSN. In the last case ConnConfig.Password stays
// zero and the DSN is passed through unchanged. Resolved secrets lose a single
// trailing "\n" or "\r\n", since file:// secrets usually end in a newline.
package secrets
