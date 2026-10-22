package snowflake

import "os"

// sdkSwitches are gosnowflake behaviours the driver turns off, through the
// only switches gosnowflake has for them, process environment variables. A
// value the user already set is kept.
//
//   - SNOWFLAKE_DISABLE_PLATFORM_DETECTION: otherwise, on the first
//     connection, gosnowflake probes the cloud metadata servers
//     (169.254.169.254, metadata.google.internal) and calls AWS STS with
//     the machine's AWS credentials, to report the platform in its login
//     telemetry. A terminal tool should not reach out to AWS because a
//     Snowflake connection was opened.
//   - SF_DISABLE_MINICORE: otherwise, in a cgo build, gosnowflake writes an
//     embedded native library into the temp directory and loads it with
//     dlopen. (tobira's release builds have no cgo, so it cannot load
//     there anyway.)
var sdkSwitches = map[string]string{
	"SNOWFLAKE_DISABLE_PLATFORM_DETECTION": "true",
	"SF_DISABLE_MINICORE":                  "true",
}

// disableSDKProbes sets each of sdkSwitches the user has not set. It runs
// once, from init.
func disableSDKProbes() {
	for k, v := range sdkSwitches {
		if _, set := os.LookupEnv(k); !set {
			_ = os.Setenv(k, v)
		}
	}
}
