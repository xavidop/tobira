package chroma

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/xavidop/tobira/driver"
)

// This file is a small typed client for the Chroma v2 REST API. The driver
// does not use github.com/amikos-tech/chroma-go: its collection handles
// build embedding functions from the configuration stored on the server
// (the ONNX default downloads a runtime and a model; provider functions
// read API keys from the environment and call out), NewHTTPClient reads
// CHROMA_TENANT and CHROMA_DATABASE from the environment, and the module
// pulls Docker, AWS and Gemini SDKs into the build. tobira needs a dozen
// endpoints and must never embed on anyone's behalf but the host's.

// maxBody caps one response body. A get page of 1000 records with
// 4096-dimensional embeddings is about 40 MiB of JSON.
const maxBody = 64 << 20

// apiClient sends requests to one Chroma server.
type apiClient struct {
	base    string // scheme://host:port
	token   string
	header  string // headerChromaToken or headerAuthorization
	http    *http.Client
	maxBody int // reply cap; 0 = maxBody
}

// apiError is a non-2xx reply: the HTTP status and Chroma's error body
// ({"error": "NotFoundError", "message": "..."}).
type apiError struct {
	status  int
	name    string
	message string
}

func (e *apiError) Error() string {
	if e.name != "" {
		return e.name + ": " + e.message
	}
	return fmt.Sprintf("HTTP %d: %s", e.status, e.message)
}

// do sends one request and decodes a 2xx JSON reply into out (nil skips
// the body). The path is under /api/v2 and already escaped.
func (a *apiClient) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+"/api/v2"+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "tobira-chroma")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if a.token != "" {
		if a.header == headerAuthorization {
			req.Header.Set("Authorization", "Bearer "+a.token)
		} else {
			req.Header.Set("X-Chroma-Token", a.token)
		}
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		where := "elsewhere"
		if loc, err := resp.Location(); err == nil {
			where = loc.Scheme + "://" + loc.Host
		}
		return fmt.Errorf("chroma answered with a redirect (HTTP %d) to %s: tobira does not follow redirects, so the token never leaves the configured host; put the final address in the DSN", resp.StatusCode, where)
	}
	limit := a.maxBody
	if limit <= 0 {
		limit = maxBody
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	if err != nil {
		return err
	}
	if len(data) > limit {
		return driver.Wrap(driver.ErrLimit, Name, "read", fmt.Errorf("the reply is larger than %s: ask for fewer records or leave out embeddings", sizeText(limit)))
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		ae := &apiError{status: resp.StatusCode}
		var eb struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		if json.Unmarshal(data, &eb) == nil && (eb.Error != "" || eb.Message != "") {
			ae.name, ae.message = eb.Error, eb.Message
		} else {
			ae.message = http.StatusText(resp.StatusCode)
		}
		return ae
	}
	if out == nil {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("unexpected reply from chroma: %w", err)
	}
	return nil
}

func sizeText(n int) string {
	if n >= 1<<20 {
		return strconv.Itoa(n>>20) + " MiB"
	}
	return strconv.Itoa(n) + " bytes"
}

// dbPath is the path of a database: /tenants/{t}/databases/{d}.
func dbPath(tenant, database string) string {
	return "/tenants/" + url.PathEscape(tenant) + "/databases/" + url.PathEscape(database)
}

type identityModel struct {
	Tenant    string   `json:"tenant"`
	Databases []string `json:"databases"`
}

func (a *apiClient) identity(ctx context.Context) (identityModel, error) {
	var out identityModel
	err := a.do(ctx, http.MethodGet, "/auth/identity", nil, &out)
	return out, err
}

type databaseModel struct {
	Name   string `json:"name"`
	Tenant string `json:"tenant"`
}

func (a *apiClient) getDatabase(ctx context.Context, tenant, database string) (databaseModel, error) {
	var out databaseModel
	err := a.do(ctx, http.MethodGet, dbPath(tenant, database), nil, &out)
	return out, err
}

func (a *apiClient) listDatabases(ctx context.Context, tenant string, limit, offset int) ([]databaseModel, error) {
	var out []databaseModel
	path := "/tenants/" + url.PathEscape(tenant) + "/databases?limit=" + strconv.Itoa(limit) + "&offset=" + strconv.Itoa(offset)
	err := a.do(ctx, http.MethodGet, path, nil, &out)
	return out, err
}

// collectionModel is a collection as the server describes it.
type collectionModel struct {
	ID            string          `json:"id"`
	Name          string          `json:"name"`
	Configuration json.RawMessage `json:"configuration_json"`
	Schema        json.RawMessage `json:"schema"`
	Metadata      json.RawMessage `json:"metadata"`
	Dimension     *int            `json:"dimension"`
}

func (a *apiClient) listCollections(ctx context.Context, tenant, database string, limit, offset int) ([]collectionModel, error) {
	var out []collectionModel
	path := dbPath(tenant, database) + "/collections?limit=" + strconv.Itoa(limit) + "&offset=" + strconv.Itoa(offset)
	err := a.do(ctx, http.MethodGet, path, nil, &out)
	return out, err
}

func (a *apiClient) getCollection(ctx context.Context, tenant, database, name string) (collectionModel, error) {
	var out collectionModel
	err := a.do(ctx, http.MethodGet, dbPath(tenant, database)+"/collections/"+url.PathEscape(name), nil, &out)
	return out, err
}

func (a *apiClient) recordsPath(tenant, database, id, op string) string {
	return dbPath(tenant, database) + "/collections/" + url.PathEscape(id) + "/" + op
}

func (a *apiClient) count(ctx context.Context, tenant, database, id string) (int64, error) {
	var out json.Number
	if err := a.do(ctx, http.MethodGet, a.recordsPath(tenant, database, id, "count"), nil, &out); err != nil {
		return 0, err
	}
	return out.Int64()
}

// getRequest is the body of /get. Empty IDs are left out (no id
// selection), so a caller whose id selection is empty must not send it.
type getRequest struct {
	IDs           []string `json:"ids,omitempty"`
	Where         any      `json:"where,omitempty"`
	WhereDocument any      `json:"where_document,omitempty"`
	Limit         *int     `json:"limit,omitempty"`
	Offset        *int     `json:"offset,omitempty"`
	Include       []string `json:"include"`
}

type getResponse struct {
	IDs        []string          `json:"ids"`
	Documents  []*string         `json:"documents"`
	URIs       []*string         `json:"uris"`
	Metadatas  []json.RawMessage `json:"metadatas"`
	Embeddings [][]json.Number   `json:"embeddings"`
}

func (a *apiClient) get(ctx context.Context, tenant, database, id string, req getRequest) (getResponse, error) {
	var out getResponse
	err := a.do(ctx, http.MethodPost, a.recordsPath(tenant, database, id, "get"), req, &out)
	return out, err
}

// queryRequest is the body of /query. Chroma's REST API takes embeddings
// only; it never embeds query text.
type queryRequest struct {
	QueryEmbeddings [][]float32 `json:"query_embeddings"`
	IDs             []string    `json:"ids,omitempty"`
	Where           any         `json:"where,omitempty"`
	WhereDocument   any         `json:"where_document,omitempty"`
	NResults        int         `json:"n_results"`
	Include         []string    `json:"include"`
}

type queryResponse struct {
	IDs        [][]string          `json:"ids"`
	Documents  [][]*string         `json:"documents"`
	URIs       [][]*string         `json:"uris"`
	Metadatas  [][]json.RawMessage `json:"metadatas"`
	Embeddings [][][]json.Number   `json:"embeddings"`
	Distances  [][]*json.Number    `json:"distances"`
}

func (a *apiClient) query(ctx context.Context, tenant, database, id string, req queryRequest) (queryResponse, error) {
	var out queryResponse
	err := a.do(ctx, http.MethodPost, a.recordsPath(tenant, database, id, "query"), req, &out)
	return out, err
}

// addRequest is the body of /add: one record (the Mutator writes one at a
// time).
type addRequest struct {
	IDs        []string         `json:"ids"`
	Embeddings [][]float32      `json:"embeddings"`
	Documents  []*string        `json:"documents,omitempty"`
	URIs       []*string        `json:"uris,omitempty"`
	Metadatas  []map[string]any `json:"metadatas,omitempty"`
}

func (a *apiClient) add(ctx context.Context, tenant, database, id string, req addRequest) error {
	return a.do(ctx, http.MethodPost, a.recordsPath(tenant, database, id, "add"), req, nil)
}

// updateRequest is the body of /update. A metadata key set to null is
// removed; keys not named are kept.
type updateRequest struct {
	IDs        []string         `json:"ids"`
	Embeddings [][]float32      `json:"embeddings,omitempty"`
	Documents  []*string        `json:"documents,omitempty"`
	URIs       []*string        `json:"uris,omitempty"`
	Metadatas  []map[string]any `json:"metadatas,omitempty"`
}

func (a *apiClient) update(ctx context.Context, tenant, database, id string, req updateRequest) error {
	return a.do(ctx, http.MethodPost, a.recordsPath(tenant, database, id, "update"), req, nil)
}

func (a *apiClient) delete(ctx context.Context, tenant, database, id string, ids []string) error {
	if len(ids) == 0 {
		return errors.New("no ids to delete")
	}
	return a.do(ctx, http.MethodPost, a.recordsPath(tenant, database, id, "delete"), map[string]any{"ids": ids}, nil)
}
