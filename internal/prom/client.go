// Package prom is a minimal client for the Prometheus HTTP query API. It can
// talk to Prometheus directly over HTTP or through the Kubernetes API
// server's service proxy.
package prom

import (
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

// Sample is one element of an instant-vector result.
type Sample struct {
	Labels map[string]string
	Value  float64
	Time   time.Time
}

// Transport performs a GET against a Prometheus API path such as
// "api/v1/query".
type Transport interface {
	Get(ctx context.Context, path string, params url.Values) ([]byte, error)
	// String describes the endpoint for messages.
	String() string
}

// Client runs instant queries.
type Client struct {
	transport Transport
}

// NewClient returns a client using the given transport.
func NewClient(t Transport) *Client { return &Client{transport: t} }

// Endpoint describes where the client sends queries.
func (c *Client) Endpoint() string { return c.transport.String() }

type apiResponse struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
	Data      struct {
		ResultType string            `json:"resultType"`
		Result     []json.RawMessage `json:"result"`
	} `json:"data"`
}

type vectorElement struct {
	Metric map[string]string  `json:"metric"`
	Value  [2]json.RawMessage `json:"value"`
}

// Query evaluates an instant query at the given time and returns the
// resulting vector. Scalar results are returned as a single unlabelled
// sample.
func (c *Client) Query(ctx context.Context, query string, at time.Time) ([]Sample, error) {
	params := url.Values{}
	params.Set("query", query)
	if !at.IsZero() {
		params.Set("time", strconv.FormatFloat(float64(at.UnixMilli())/1000, 'f', 3, 64))
	}
	body, err := c.transport.Get(ctx, "api/v1/query", params)
	if err != nil {
		return nil, err
	}
	return parseQueryResponse(body)
}

// Ping checks that the endpoint answers queries like a Prometheus server.
func (c *Client) Ping(ctx context.Context) error {
	res, err := c.Query(ctx, "vector(1)", time.Time{})
	if err != nil {
		return err
	}
	if len(res) != 1 || res[0].Value != 1 {
		return fmt.Errorf("unexpected answer to probe query from %s", c.Endpoint())
	}
	return nil
}

func parseQueryResponse(body []byte) ([]Sample, error) {
	var resp apiResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("response is not Prometheus API JSON: %w (body: %s)", err, snippet(body))
	}
	if resp.Status != "success" {
		if resp.Error != "" {
			return nil, fmt.Errorf("prometheus returned %s: %s", orDefault(resp.ErrorType, "error"), resp.Error)
		}
		return nil, fmt.Errorf("response is not a Prometheus API result (body: %s)", snippet(body))
	}
	switch resp.Data.ResultType {
	case "vector":
		out := make([]Sample, 0, len(resp.Data.Result))
		for _, raw := range resp.Data.Result {
			var el vectorElement
			if err := json.Unmarshal(raw, &el); err != nil {
				return nil, fmt.Errorf("decoding vector element: %w", err)
			}
			ts, v, err := parsePoint(el.Value)
			if err != nil {
				return nil, err
			}
			if el.Metric == nil {
				el.Metric = map[string]string{}
			}
			out = append(out, Sample{Labels: el.Metric, Value: v, Time: ts})
		}
		return out, nil
	case "scalar":
		if len(resp.Data.Result) != 2 {
			return nil, fmt.Errorf("malformed scalar result")
		}
		ts, v, err := parsePoint([2]json.RawMessage{resp.Data.Result[0], resp.Data.Result[1]})
		if err != nil {
			return nil, err
		}
		return []Sample{{Labels: map[string]string{}, Value: v, Time: ts}}, nil
	default:
		return nil, fmt.Errorf("unsupported result type %q (want an instant vector)", resp.Data.ResultType)
	}
}

// prometheusError returns the error carried by a Prometheus API error
// body, or nil if the body is something else (an HTML error page, a
// Kubernetes Status object, ...).
func prometheusError(body []byte) error {
	var resp apiResponse
	if err := json.Unmarshal(body, &resp); err != nil || resp.Status != "error" || resp.Error == "" {
		return nil
	}
	return fmt.Errorf("prometheus returned %s: %s", orDefault(resp.ErrorType, "error"), resp.Error)
}

func parsePoint(p [2]json.RawMessage) (time.Time, float64, error) {
	var ts float64
	if err := json.Unmarshal(p[0], &ts); err != nil {
		return time.Time{}, 0, fmt.Errorf("decoding sample timestamp: %w", err)
	}
	var s string
	if err := json.Unmarshal(p[1], &s); err != nil {
		return time.Time{}, 0, fmt.Errorf("decoding sample value: %w", err)
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("decoding sample value %q: %w", s, err)
	}
	return time.UnixMilli(int64(ts * 1000)), v, nil
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// HTTPTransport talks to a Prometheus-compatible server at a base URL.
type HTTPTransport struct {
	base   *url.URL
	client *http.Client
	token  string
}

// NewHTTPTransport returns a transport for the given base URL, for example
// "http://localhost:9090" or "https://prom.example.com/prometheus". The
// bearer token is optional.
func NewHTTPTransport(rawURL, bearerToken string, timeout time.Duration) (*HTTPTransport, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, fmt.Errorf("invalid Prometheus URL %q: %w", rawURL, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("invalid Prometheus URL %q: want http(s)://host[:port][/prefix]", rawURL)
	}
	u.RawQuery, u.Fragment = "", ""
	return &HTTPTransport{base: u, client: &http.Client{Timeout: timeout}, token: strings.TrimSpace(bearerToken)}, nil
}

// Get implements Transport.
func (t *HTTPTransport) Get(ctx context.Context, path string, params url.Values) ([]byte, error) {
	u := *t.base
	u.Path = strings.TrimRight(u.Path, "/") + "/" + strings.TrimLeft(path, "/")
	u.RawQuery = params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if t.token != "" {
		req.Header.Set("Authorization", "Bearer "+t.token)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("querying %s: %w", t, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return nil, fmt.Errorf("reading response from %s: %w", t, err)
	}
	if resp.StatusCode/100 != 2 {
		// Prometheus reports query errors as JSON with a 4xx/5xx status.
		if perr := prometheusError(body); perr != nil {
			return nil, perr
		}
		return nil, fmt.Errorf("%s answered HTTP %d: %s", t, resp.StatusCode, snippet(body))
	}
	return body, nil
}

func (t *HTTPTransport) String() string { return t.base.Redacted() }
