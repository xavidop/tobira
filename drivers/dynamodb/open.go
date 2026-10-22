package dynamodb

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodbstreams"
	"github.com/aws/smithy-go/logging"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/netpolicy"
)

// openProbeBackoff caps the backoff between retries of Open's ListTables
// probe.
const openProbeBackoff = 500 * time.Millisecond

// settings is what a DSN says. The secret stays a driver.Secret so it
// never prints.
type settings struct {
	region      string
	endpoint    string
	profile     string
	accessKeyID string
	secret      driver.Secret
}

var regionRE = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// parseDSN reads dynamodb://[access_key_id[:secret]@][region][?endpoint=…&profile=…&allow_plaintext=…].
// An http endpoint to a host that is not loopback also needs
// allow_plaintext=1, and AWS's own endpoints (*.amazonaws.com,
// *.amazonaws.com.cn) are always https.
// A non-empty ConnConfig.Password is the secret access key and wins over
// one in the DSN. An access key id needs a secret and a secret needs an
// access key id; a profile excludes both. Unknown parameters are refused.
// Errors never quote the DSN, which may hold the secret.
func parseDSN(cfg driver.ConnConfig) (settings, error) {
	fail := func(format string, args ...any) (settings, error) {
		return settings{}, driver.Wrap(driver.ErrSyntax, Name, "open", fmt.Errorf(format, args...))
	}
	if !strings.EqualFold(driver.Scheme(cfg.DSN), "dynamodb") {
		return fail("the DSN must be a dynamodb:// URL")
	}
	u, err := url.Parse(cfg.DSN)
	if err != nil {
		return fail("the DSN is not a valid URL (check the escaping of its user and parameters)")
	}
	if u.Opaque != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return fail("the DSN is dynamodb://[access_key_id@]region[?endpoint=…&profile=…], with no path")
	}
	if u.Port() != "" {
		return fail("the DSN host is a region and takes no port: put a local endpoint in ?endpoint=")
	}
	var st settings
	st.region = strings.ToLower(u.Hostname())
	if st.region != "" && !regionRE.MatchString(st.region) {
		return fail("invalid region %q", st.region)
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return fail("the DSN parameters are not valid (check their escaping)")
	}
	allowPlain := false
	for _, k := range slices.Sorted(maps.Keys(q)) {
		vs := q[k]
		if len(vs) != 1 || vs[0] == "" {
			return fail("DSN parameter %q must appear once, with a value", k)
		}
		switch k {
		case "endpoint":
			if err := checkEndpoint(vs[0]); err != nil {
				return settings{}, err
			}
			st.endpoint = vs[0]
		case "profile":
			st.profile = vs[0]
		case "allow_plaintext":
			b, err := strconv.ParseBool(vs[0])
			if err != nil {
				return fail("allow_plaintext must be true or false")
			}
			allowPlain = b
		default:
			return fail("unknown DSN parameter %q: use endpoint, profile or allow_plaintext", k)
		}
	}
	if plainRemote(st.endpoint) {
		switch netpolicy.CheckPlaintext(endpointHostname(st.endpoint), allowPlain, awsZones...) {
		case netpolicy.RefusedCloud:
			return fail("an http endpoint is refused for AWS's own hosts (*.amazonaws.com, *.amazonaws.com.cn): use https")
		case netpolicy.NeedsOptIn:
			return fail("an http endpoint to a host that is not loopback needs allow_plaintext=1")
		}
	}
	dsnSecret := ""
	if u.User != nil {
		st.accessKeyID = u.User.Username()
		dsnSecret, _ = u.User.Password()
	}
	switch {
	case !cfg.Password.IsZero():
		st.secret = cfg.Password
	case dsnSecret != "":
		st.secret = driver.NewSecret(dsnSecret)
	}
	switch {
	case st.accessKeyID != "" && st.secret.IsZero():
		return fail("the access key id needs its secret access key: give it through the password reference")
	case st.accessKeyID == "" && !st.secret.IsZero():
		return fail("a password (secret access key) needs the access key id as the DSN user: dynamodb://ACCESS_KEY_ID@region")
	case st.profile != "" && st.accessKeyID != "":
		return fail("use either a profile or an access key, not both")
	}
	return st, nil
}

// checkEndpoint accepts an http or https URL with a host and no
// credentials, query or fragment.
func checkEndpoint(raw string) error {
	fail := func(msg string) error {
		return driver.Wrap(driver.ErrSyntax, Name, "open", errors.New("invalid endpoint: "+msg))
	}
	e, err := url.Parse(raw)
	switch {
	case err != nil:
		return fail("not a URL")
	case e.Scheme != "http" && e.Scheme != "https":
		return fail("the scheme must be http or https")
	case e.Host == "":
		return fail("no host")
	case e.User != nil:
		return fail("credentials do not belong in the endpoint")
	case e.RawQuery != "" || e.Fragment != "" || e.ForceQuery:
		return fail("no query or fragment allowed")
	}
	return nil
}

// Open loads the AWS configuration (the default chain, the DSN's profile,
// or the DSN's access key with the password as its secret), retrieves the
// credentials once, so missing ones fail here, and lists one table, so an
// unreachable endpoint or rejected credentials fail here too. The SDK
// never logs.
func (ddbDriver) Open(ctx context.Context, cfg driver.ConnConfig) (driver.Conn, error) {
	const op = "open"
	if len(cfg.Options) > 0 {
		k := slices.Sorted(maps.Keys(cfg.Options))[0]
		return nil, driver.Wrap(driver.ErrUnsupported, Name, op, fmt.Errorf("unsupported option %q: the dynamodb driver takes its settings from the DSN", k))
	}
	st, err := parseDSN(cfg)
	if err != nil {
		return nil, err
	}
	c := &conn{readOnly: cfg.ReadOnly, limits: defaultLimits, secrets: secretList(st.secret.Reveal()), done: make(chan struct{})}
	c.transport = http.DefaultTransport.(*http.Transport).Clone()
	opts := []func(*config.LoadOptions) error{
		// No redirect is followed: Go turns a POST into a GET on a 301,
		// 302 or 303 and follows it, keeping Authorization (the SigV4
		// signature) on the same host name and forwarding
		// X-Amz-Security-Token to any host.
		config.WithHTTPClient(&http.Client{Transport: closingTransport{c}, CheckRedirect: netpolicy.RefuseRedirects}),
		config.WithLogger(logging.Nop{}),
		config.WithClientLogMode(0),
		config.WithLogConfigurationWarnings(false),
		config.WithAppID("tobira"),
	}
	if st.region != "" {
		opts = append(opts, config.WithRegion(st.region))
	}
	if st.profile != "" {
		opts = append(opts, config.WithSharedConfigProfile(st.profile))
	}
	if st.accessKeyID != "" {
		opts = append(opts, config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(st.accessKeyID, st.secret.Reveal(), "")))
	}
	awsCfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, c.openFail(ctx, driver.ErrAuth, fmt.Errorf("load the AWS configuration: %w", err))
	}
	if awsCfg.Region == "" {
		return nil, c.openFail(ctx, driver.ErrSyntax, errors.New("no region: name it as the DSN host (dynamodb://us-east-1) or set AWS_REGION"))
	}
	if awsCfg.Credentials == nil {
		return nil, c.openFail(ctx, driver.ErrAuth, errors.New("no AWS credentials found"))
	}
	creds, err := awsCfg.Credentials.Retrieve(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, c.openFail(ctx, nil, err)
		}
		return nil, c.openFail(ctx, driver.ErrAuth, fmt.Errorf("no usable AWS credentials: %w", err))
	}
	c.secrets = secretList(st.secret.Reveal(), creds.SecretAccessKey, creds.SessionToken)
	c.region = awsCfg.Region
	c.db = dynamodb.NewFromConfig(awsCfg, func(o *dynamodb.Options) {
		if st.endpoint != "" {
			o.BaseEndpoint = aws.String(st.endpoint)
		}
	})
	c.streams = dynamodbstreams.NewFromConfig(awsCfg, func(o *dynamodbstreams.Options) {
		if st.endpoint != "" {
			o.BaseEndpoint = aws.String(st.endpoint)
		}
	})
	if plainRemote(st.endpoint) {
		cfg.Log().Warn("dynamodb: the endpoint uses plain http; requests are signed but not encrypted", "host", endpointHost(st.endpoint))
	}
	c.catalog = newCatalogCache(c)
	// The probe keeps the SDK's retries but caps their backoff, so an
	// endpoint that refuses connections fails Open in about a second, not
	// after the default backoff of up to 6s.
	probeRetry := func(o *dynamodb.Options) { o.Retryer = retry.AddWithMaxBackoffDelay(o.Retryer, openProbeBackoff) }
	if _, err := c.db.ListTables(ctx, &dynamodb.ListTablesInput{Limit: aws.Int32(1)}, probeRetry); err != nil {
		c.transport.CloseIdleConnections()
		return nil, c.fail(ctx, op, err)
	}
	return c, nil
}

// openFail builds an open error of the given kind, masks the secrets in it
// and releases the transport.
func (c *conn) openFail(ctx context.Context, kind error, err error) error {
	c.transport.CloseIdleConnections()
	if ctx.Err() != nil {
		return c.fail(ctx, "open", err)
	}
	return c.scrub(driver.Wrap(kind, Name, "open", err))
}

// plainRemote reports whether an endpoint is plain http to a host that is
// not the loopback interface.
func plainRemote(endpoint string) bool {
	e, err := url.Parse(endpoint)
	if err != nil || e.Scheme != "http" {
		return false
	}
	return !netpolicy.IsLoopback(e.Hostname())
}

// awsZones are AWS's own endpoints (global and China regions), which are
// never reached in clear.
var awsZones = []string{"amazonaws.com", "amazonaws.com.cn"}

func endpointHostname(endpoint string) string {
	e, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	return e.Hostname()
}

func endpointHost(endpoint string) string {
	e, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	return e.Host
}
