package prom

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestClient(t *testing.T, h http.HandlerFunc, token string) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	tr, err := NewHTTPTransport(srv.URL+"/prefix/", token, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return NewClient(tr), srv
}

func TestQueryVector(t *testing.T) {
	at := time.Unix(1700000000, 0)
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/prefix/api/v1/query" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("query"); got != `up{job="x"}` {
			t.Errorf("query = %q", got)
		}
		if got := r.URL.Query().Get("time"); got != "1700000000.000" {
			t.Errorf("time = %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer s3cret" {
			t.Errorf("Authorization = %q", got)
		}
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[
			{"metric":{"pod":"a","namespace":"ns"},"value":[1700000000.5,"42.5"]},
			{"metric":{},"value":[1700000000,"NaN"]}]}}`)
	}, " s3cret\n")
	got, err := c.Query(context.Background(), `up{job="x"}`, at)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Value != 42.5 || got[0].Labels["pod"] != "a" {
		t.Fatalf("unexpected result: %+v", got)
	}
	if got[0].Time.UnixMilli() != 1700000000500 {
		t.Errorf("timestamp = %v", got[0].Time)
	}
	if got[1].Value == got[1].Value { // NaN != NaN
		t.Errorf("want NaN, got %v", got[1].Value)
	}
}

func TestQueryScalarAndPing(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"scalar","result":[1700000000,"1"]}}`)
	}, "")
	if err := c.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestQueryErrors(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{"prometheus error", 422, `{"status":"error","errorType":"execution","error":"query timed out"}`, "prometheus returned execution: query timed out"},
		{"error with 200", 200, `{"status":"error","errorType":"bad_data","error":"parse error"}`, "bad_data: parse error"},
		{"html error page", 502, `<html>Bad Gateway</html>`, "HTTP 502"},
		{"not json", 200, `hello`, "not Prometheus API JSON"},
		{"other json", 200, `{"kind":"Status"}`, "not a Prometheus API result"},
		{"matrix", 200, `{"status":"success","data":{"resultType":"matrix","result":[]}}`, "unsupported result type"},
		{"bad value", 200, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"abc"]}]}}`, "decoding sample value"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				fmt.Fprint(w, tt.body)
			}, "")
			_, err := c.Query(context.Background(), "up", time.Time{})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestNewHTTPTransportValidation(t *testing.T) {
	for _, bad := range []string{"", "localhost:9090", "ftp://x", "http://", "::"} {
		if _, err := NewHTTPTransport(bad, "", time.Second); err == nil {
			t.Errorf("NewHTTPTransport(%q) should fail", bad)
		}
	}
	tr, err := NewHTTPTransport("https://user:pw@prom.example.com/p?x=1", "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if s := tr.String(); strings.Contains(s, "pw") || strings.Contains(s, "x=1") {
		t.Errorf("String() leaks credentials or query: %s", s)
	}
}

func TestUnreachable(t *testing.T) {
	tr, err := NewHTTPTransport("http://127.0.0.1:1", "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewClient(tr).Query(context.Background(), "up", time.Time{}); err == nil {
		t.Fatal("expected connection error")
	}
}
