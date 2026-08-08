// Package imageio is a client for ovirt-imageio, the data plane of oVirt disk
// transfers. The engine hands out a ticket URL; everything here speaks to that
// URL directly.
package imageio

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client talks to one imageio ticket.
type Client struct {
	base string
	http *http.Client
}

// New wraps a transfer URL. The HTTP client should carry the engine CA, which
// is why it is passed in rather than built here.
func New(transferURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Minute}
	}
	return &Client{base: strings.TrimRight(transferURL, "/"), http: httpClient}
}

// URL returns the ticket URL in use.
func (c *Client) URL() string { return c.base }

// Features is the capability document returned by OPTIONS.
type Features struct {
	Features   []string `json:"features"`
	UnixSocket string   `json:"unix_socket,omitempty"`
	MaxReaders int      `json:"max_readers,omitempty"`
	MaxWriters int      `json:"max_writers,omitempty"`
}

// Has reports whether a named capability is present.
func (f *Features) Has(name string) bool {
	for _, s := range f.Features {
		if s == name {
			return true
		}
	}
	return false
}

// Options queries the ticket's capabilities. Older daemons answer 405 to
// OPTIONS on the ticket path; that is reported as "no optional features"
// rather than as an error, because plain ranged reads still work.
func (c *Client) Options(ctx context.Context) (*Features, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodOptions, c.base, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("imageio OPTIONS: %w", err)
	}
	defer drain(resp)

	if resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusNotImplemented {
		return &Features{}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, errorFrom(resp, "OPTIONS", c.base)
	}

	var features Features
	if err := json.NewDecoder(resp.Body).Decode(&features); err != nil {
		return nil, fmt.Errorf("разбор возможностей imageio: %w", err)
	}
	return &features, nil
}

// Extent is one contiguous region of the image.
type Extent struct {
	Start  int64 `json:"start"`
	Length int64 `json:"length"`
	// Zero — область читается как нули, данных передавать не нужно.
	Zero bool `json:"zero"`
	// Hole — область не выделена в образе (частный случай Zero).
	Hole bool `json:"hole"`
	// Dirty — область изменилась с момента опорного checkpoint
	// (только для context=dirty).
	Dirty bool `json:"dirty"`
}

// End returns the exclusive end offset of the extent.
func (e Extent) End() int64 { return e.Start + e.Length }

// Extent contexts.
const (
	// ContextZero описывает, какие области содержат данные, а какие — нули.
	// Используется для полного бэкапа: разреженные тома читаются только там,
	// где что-то записано.
	ContextZero = "zero"
	// ContextDirty описывает области, изменившиеся с опорного checkpoint.
	// Доступен только для передач, открытых на бэкап с from_checkpoint_id.
	ContextDirty = "dirty"
)

// Extents fetches the extent map for the requested context.
func (c *Client) Extents(ctx context.Context, extentContext string) ([]Extent, error) {
	q := url.Values{}
	if extentContext != "" {
		q.Set("context", extentContext)
	}
	endpoint := c.base + "/extents"
	if len(q) > 0 {
		endpoint += "?" + q.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("imageio extents (%s): %w", extentContext, err)
	}
	defer drain(resp)

	if resp.StatusCode != http.StatusOK {
		return nil, errorFrom(resp, "GET", endpoint)
	}

	var extents []Extent
	if err := json.NewDecoder(resp.Body).Decode(&extents); err != nil {
		return nil, fmt.Errorf("разбор карты экстентов: %w", err)
	}
	return extents, nil
}

// ReadRange copies [offset, offset+length) into w and returns how many bytes
// were written.
func (c *Client) ReadRange(ctx context.Context, offset, length int64, w io.Writer) (int64, error) {
	if length <= 0 {
		return 0, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base, nil)
	if err != nil {
		return 0, err
	}
	// HTTP ranges are inclusive on both ends.
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, offset+length-1))

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("imageio чтение %d+%d: %w", offset, length, err)
	}
	defer drain(resp)

	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		return 0, errorFrom(resp, "GET", c.base)
	}

	n, err := io.Copy(w, resp.Body)
	if err != nil {
		return n, fmt.Errorf("imageio чтение тела %d+%d: %w", offset, length, err)
	}
	if n != length {
		return n, fmt.Errorf("imageio вернул %d байт вместо %d по смещению %d", n, length, offset)
	}
	return n, nil
}

// WriteRange uploads length bytes from r at the given offset.
func (c *Client) WriteRange(ctx context.Context, offset int64, r io.Reader, length int64, flush bool) error {
	if length <= 0 {
		return nil
	}
	endpoint := c.base
	if !flush {
		// Flushing on every chunk destroys throughput; the caller issues one
		// explicit Flush at the end instead.
		endpoint += "?flush=n"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, r)
	if err != nil {
		return err
	}
	req.ContentLength = length
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/*", offset, offset+length-1))

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("imageio запись %d+%d: %w", offset, length, err)
	}
	defer drain(resp)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return errorFrom(resp, "PUT", endpoint)
	}
	return nil
}

// Zero punches a zero range without transferring the data. Restoring a sparse
// image over an existing disk needs this to erase whatever was there before.
func (c *Client) Zero(ctx context.Context, offset, size int64, flush bool) error {
	if size <= 0 {
		return nil
	}
	body, err := json.Marshal(map[string]any{
		"op":     "zero",
		"offset": offset,
		"size":   size,
		"flush":  flush,
	})
	if err != nil {
		return err
	}
	return c.patch(ctx, body)
}

// Flush forces buffered writes to stable storage.
func (c *Client) Flush(ctx context.Context) error {
	body, err := json.Marshal(map[string]any{"op": "flush"})
	if err != nil {
		return err
	}
	return c.patch(ctx, body)
}

func (c *Client) patch(ctx context.Context, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, c.base, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("imageio PATCH: %w", err)
	}
	defer drain(resp)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return errorFrom(resp, "PATCH", c.base)
	}
	return nil
}

// Checksum is the daemon-side digest of the image.
type Checksum struct {
	Checksum  string `json:"checksum"`
	Algorithm string `json:"algorithm"`
	BlockSize int64  `json:"block_size"`
}

// ChecksumOf asks the daemon to hash the image it is serving. Comparing this
// against a digest computed over what we stored is the strongest verification
// available: it is calculated independently, on the other side of the wire.
//
// The digest is block-based, so it is only comparable against a digest computed
// with the same algorithm and block size.
func (c *Client) ChecksumOf(ctx context.Context, algorithm string, blockSize int64) (*Checksum, error) {
	q := url.Values{}
	if algorithm != "" {
		q.Set("algorithm", algorithm)
	}
	if blockSize > 0 {
		q.Set("block_size", strconv.FormatInt(blockSize, 10))
	}
	endpoint := c.base + "/checksum"
	if len(q) > 0 {
		endpoint += "?" + q.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("imageio checksum: %w", err)
	}
	defer drain(resp)

	if resp.StatusCode != http.StatusOK {
		return nil, errorFrom(resp, "GET", endpoint)
	}

	var sum Checksum
	if err := json.NewDecoder(resp.Body).Decode(&sum); err != nil {
		return nil, fmt.Errorf("разбор контрольной суммы: %w", err)
	}
	return &sum, nil
}

// Algorithms lists the digests the daemon can compute.
func (c *Client) Algorithms(ctx context.Context) ([]string, error) {
	endpoint := c.base + "/checksum/algorithms"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer drain(resp)

	if resp.StatusCode != http.StatusOK {
		return nil, errorFrom(resp, "GET", endpoint)
	}
	var out struct {
		Algorithms []string `json:"algorithms"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Algorithms, nil
}

// Error is a non-2xx answer from the daemon.
type Error struct {
	Status int
	Method string
	URL    string
	Body   string
}

func (e *Error) Error() string {
	body := strings.TrimSpace(e.Body)
	if len(body) > 300 {
		body = body[:300] + "…"
	}
	return fmt.Sprintf("imageio %s %s: HTTP %d: %s", e.Method, redactTicket(e.URL), e.Status, body)
}

func errorFrom(resp *http.Response, method, endpoint string) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	return &Error{Status: resp.StatusCode, Method: method, URL: endpoint, Body: string(body)}
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
}

// redactTicket strips the ticket id from a URL before it reaches a log: it is
// a bearer credential for the disk's data for as long as the transfer lives.
func redactTicket(raw string) string {
	idx := strings.Index(raw, "/images/")
	if idx < 0 {
		return raw
	}
	rest := raw[idx+len("/images/"):]
	tail := ""
	if slash := strings.IndexAny(rest, "/?"); slash >= 0 {
		tail = rest[slash:]
	}
	return raw[:idx] + "/images/<ticket>" + tail
}
