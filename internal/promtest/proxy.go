package promtest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"

	restclient "k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
)

// ProxyCall records one request made through the fake service proxy.
type ProxyCall struct {
	Namespace, Scheme, Name, Port, Path string
}

// Proxy emulates the API server's service proxy for the client-go fake
// clientset: requests to registered services are served by an http.Handler.
type Proxy struct {
	mu       sync.Mutex
	backends map[string]http.Handler
	calls    []ProxyCall
}

// NewProxy returns an empty proxy.
func NewProxy() *Proxy { return &Proxy{backends: map[string]http.Handler{}} }

// Register routes namespace/name to the handler.
func (p *Proxy) Register(namespace, name string, h http.Handler) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.backends[namespace+"/"+name] = h
}

// Calls returns the requests seen so far.
func (p *Proxy) Calls() []ProxyCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]ProxyCall(nil), p.calls...)
}

// React is a k8stesting.ProxyReactionFunc; install it with
// clientset.AddProxyReactor("services", proxy.React).
func (p *Proxy) React(action k8stesting.Action) (bool, restclient.ResponseWrapper, error) {
	get, ok := action.(k8stesting.ProxyGetAction)
	if !ok {
		return false, nil, nil
	}
	call := ProxyCall{Namespace: action.GetNamespace(), Scheme: get.GetScheme(), Name: get.GetName(), Port: get.GetPort(), Path: get.GetPath()}
	p.mu.Lock()
	p.calls = append(p.calls, call)
	backend := p.backends[call.Namespace+"/"+call.Name]
	p.mu.Unlock()

	params := url.Values{}
	for k, v := range get.GetParams() {
		params.Set(k, v)
	}
	return true, &response{backend: backend, call: call, params: params}, nil
}

type response struct {
	backend http.Handler
	call    ProxyCall
	params  url.Values
}

func (r *response) DoRaw(ctx context.Context) ([]byte, error) {
	if r.backend == nil {
		return nil, fmt.Errorf("services %q not found or has no endpoints", r.call.Name)
	}
	req := httptest.NewRequest(http.MethodGet, "/"+r.call.Path+"?"+r.params.Encode(), nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	r.backend.ServeHTTP(rec, req)
	body := rec.Body.Bytes()
	if rec.Code/100 != 2 {
		return body, fmt.Errorf("the server rejected our request (status %d)", rec.Code)
	}
	return body, nil
}

func (r *response) Stream(ctx context.Context) (io.ReadCloser, error) {
	body, err := r.DoRaw(ctx)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(body)), nil
}
