package pinecone

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/pinecone-io/go-pinecone/v7/pinecone"
	"google.golang.org/grpc"

	"github.com/xavidop/tobira/driver"
	"github.com/xavidop/tobira/driverutil/netpolicy"
)

// maxRecvBytes caps one gRPC response: a page of high-dimensional vectors
// fits, a runaway one does not.
const maxRecvBytes = 64 << 20

// defaultNamespace names Pinecone's default namespace as a collection; the
// API spells it "" or "__default__".
const defaultNamespace = "__default__"

// pineconeNamespace is the namespace a collection name addresses.
func pineconeNamespace(collection string) string {
	if collection == defaultNamespace {
		return ""
	}
	return collection
}

// collectionName is the collection a namespace is shown as.
func collectionName(ns string) string {
	if ns == "" {
		return defaultNamespace
	}
	return ns
}

// indexHandle is an open index: its data-plane connection plus what the
// control plane (or the DSN) said about it. It is immutable once cached.
type indexHandle struct {
	name   string
	host   string
	conn   *pinecone.IndexConnection
	metric string // "" = unknown until the statistics report it
	dim    int    // 0 = unknown, or a sparse index
	sparse bool
}

// ns returns a connection to namespace ns of the index (sharing its gRPC
// connection).
func (h *indexHandle) ns(ns string) *pinecone.IndexConnection { return h.conn.WithNamespace(ns) }

// index returns the handle of index name ("" = the DSN's index host),
// opening and caching it on first use. In host mode only the DSN's index
// exists; otherwise the control plane names its host. A missing index is
// ErrNotFound.
func (c *conn) index(ctx context.Context, op, name string) (*indexHandle, error) {
	if err := c.check(ctx, op); err != nil {
		return nil, err
	}
	if name == "" {
		if c.dsn.host == "" {
			return nil, driver.Wrap(driver.ErrSyntax, Name, op, errors.New(`name the index: this connection is a project with several indexes`))
		}
		name = c.dsn.index
	}
	c.mu.Lock()
	h, ok := c.indexes[name]
	c.mu.Unlock()
	if ok {
		return h, nil
	}
	h = &indexHandle{name: name}
	if c.dsn.host != "" {
		if name != c.dsn.index {
			return nil, driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf("index %q does not exist: this connection is the index host of %q", name, c.dsn.index))
		}
		h.host, h.metric = c.dsn.host, c.dsn.metric
	} else {
		idx, err := c.client.DescribeIndex(ctx, name)
		if err != nil {
			return nil, c.fail(ctx, op, err)
		}
		if idx == nil || idx.Host == "" {
			return nil, driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf("index %q has no host yet (is it ready?)", name))
		}
		host, err := indexHostURL(idx.Host, strings.HasPrefix(c.dsn.control, "http://"))
		if err != nil {
			return nil, driver.Wrap(driver.ErrSyntax, Name, op, err)
		}
		h.host = host
		h.metric, h.dim, h.sparse = schemaVectors(idx.Schema)
	}
	ic, err := c.client.Index(pinecone.NewIndexConnParams{Host: h.host}, grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxRecvBytes)))
	if err != nil {
		return nil, c.fail(ctx, op, err)
	}
	h.conn = ic
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.indexes == nil { // closed meanwhile
		_ = ic.Close()
		return nil, driver.Wrap(driver.ErrConnection, Name, op, errClosed)
	}
	if prev, ok := c.indexes[name]; ok { // another call won the race
		_ = ic.Close()
		return prev, nil
	}
	c.indexes[name] = h
	return h, nil
}

// indexInfo is what one statistics call says about an index.
type indexInfo struct {
	metric     string
	dim        int
	sparse     bool
	namespaces map[string]uint32 // collection name -> record count
}

// stats reads the index statistics, filling in what the handle does not
// know.
func (c *conn) stats(ctx context.Context, op string, h *indexHandle) (indexInfo, error) {
	st, err := h.conn.DescribeIndexStats(ctx)
	if err != nil {
		return indexInfo{}, c.fail(ctx, op, err)
	}
	info := indexInfo{metric: h.metric, dim: h.dim, sparse: h.sparse, namespaces: map[string]uint32{}}
	if info.metric == "" && st.Metric != nil {
		info.metric = string(*st.Metric)
	}
	if st.Dimension != nil && *st.Dimension > 0 {
		info.dim = int(*st.Dimension)
	}
	if st.VectorType != nil {
		info.sparse = *st.VectorType == "sparse"
	} else if info.dim == 0 {
		info.sparse = true
	}
	for ns, sum := range st.Namespaces {
		n := uint32(0)
		if sum != nil {
			n = sum.VectorCount
		}
		info.namespaces[collectionName(ns)] = n
	}
	return info, nil
}

// collection resolves a collection ref: its index handle and, with check,
// the statistics, failing with ErrNotFound when the namespace does not
// exist.
func (c *conn) collection(ctx context.Context, op string, ref driver.CollectionRef, check bool) (*indexHandle, indexInfo, error) {
	h, err := c.index(ctx, op, ref.Namespace)
	if err != nil {
		return nil, indexInfo{}, err
	}
	if ref.Name == "" {
		return nil, indexInfo{}, driver.Wrap(driver.ErrSyntax, Name, op, errors.New("name the namespace (the collection)"))
	}
	if !check {
		return h, indexInfo{}, nil
	}
	info, err := c.stats(ctx, op, h)
	if err != nil {
		return nil, indexInfo{}, err
	}
	if _, ok := info.namespaces[ref.Name]; !ok {
		return nil, indexInfo{}, driver.Wrap(driver.ErrNotFound, Name, op, fmt.Errorf("namespace %q does not exist in index %q", ref.Name, h.name))
	}
	return h, info, nil
}

// schemaVectors reads the metric, dimension and vector type from an index
// schema: its one dense vector field, or else a sparse one (always
// dotproduct). Several dense fields leave them unknown.
func schemaVectors(s *pinecone.IndexSchema) (metric string, dim int, sparse bool) {
	if s == nil {
		return "", 0, false
	}
	var dense *pinecone.DenseVectorField
	hasSparse := false
	for _, f := range s.Fields {
		switch {
		case f.DenseVector != nil && dense != nil:
			return "", 0, false
		case f.DenseVector != nil:
			dense = f.DenseVector
		case f.SparseVector != nil:
			hasSparse = true
		}
	}
	switch {
	case dense != nil:
		return string(dense.Metric), int(dense.Dimension), false
	case hasSparse:
		return metricDot, 0, true
	}
	return "", 0, false
}

// indexHostURL is the URL of an index host the control plane names. It is
// reached over TLS unless the control plane itself was reached in clear and
// the index host is this machine (pinecone-local): a scheme in the answer
// never downgrades it, so the API key never crosses the network in clear.
func indexHostURL(host string, plaintext bool) (string, error) {
	bare, secure := host, false
	switch {
	case strings.HasPrefix(host, "https://"):
		bare, secure = strings.TrimPrefix(host, "https://"), true
	case strings.HasPrefix(host, "http://"):
		bare = strings.TrimPrefix(host, "http://")
	case strings.Contains(host, "://"):
		return "", fmt.Errorf("the control plane named an index host with an unknown scheme")
	}
	if bare == "" || strings.ContainsAny(bare, "/?#@") {
		return "", fmt.Errorf("the control plane named an invalid index host")
	}
	name, _, err := net.SplitHostPort(bare)
	if err != nil {
		name = bare
	}
	if plaintext && !secure && netpolicy.IsLoopback(name) {
		return "http://" + bare, nil
	}
	return "https://" + bare, nil
}
