// Package promtest provides a fake Prometheus HTTP API for tests. It
// understands exactly the query shapes kubectl-gpu-fit generates and answers
// them from a table of pre-aggregated series.
package promtest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"sync"
	"time"
)

// Series is one time series of a metric with the aggregates the fake
// returns for it. Values are in the metric's own unit.
type Series struct {
	Metric string
	Labels map[string]string
	Avg    float64
	Max    float64
	P95    float64
	// RecentMax is returned for max_over_time over a range shorter than
	// the main window.
	RecentMax float64
	// LastActive is the last time the series exceeded the idle threshold;
	// zero means never.
	LastActive time.Time
	// Samples is the count_over_time result (number of subquery steps).
	Samples float64
}

// Server is a fake Prometheus.
type Server struct {
	*httptest.Server

	// Window is the main range selector, e.g. "7d". max_over_time over any
	// other range is treated as the "recent" query.
	Window string

	mu      sync.Mutex
	series  []Series
	queries []string
	fail    *regexp.Regexp
}

var (
	simpleRe   = regexp.MustCompile(`^(avg_over_time|max_over_time)\((\w+)\[(\w+)\]\)$`)
	quantileRe = regexp.MustCompile(`^quantile_over_time\(0\.95, (\w+)\[(\w+)\]\)$`)
	lastRe     = regexp.MustCompile(`^max_over_time\(timestamp\((\w+) > ([0-9.]+)\)\[(\w+):(\w+)\]\)$`)
	countRe    = regexp.MustCompile(`^count_over_time\((\w+)\[(\w+):(\w+)\]\)$`)
)

// New starts a fake Prometheus serving the given series.
func New(window string, series []Series) *Server {
	s := &Server{Window: window, series: series}
	s.Server = httptest.NewServer(s.Handler())
	return s
}

// Handler returns the HTTP handler, for mounting behind other fakes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/query", s.handleQuery)
	return mux
}

// FailQueries makes queries matching re fail with a 422; nil turns failures
// off again.
func (s *Server) FailQueries(re *regexp.Regexp) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = re
}

// Queries returns every query received so far.
func (s *Server) Queries() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.queries...)
}

type result struct {
	Metric map[string]string `json:"metric"`
	Value  [2]any            `json:"value"`
}

func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	query := r.FormValue("query")
	s.mu.Lock()
	s.queries = append(s.queries, query)
	fail := s.fail
	s.mu.Unlock()

	ts := float64(time.Now().Unix())
	if t := r.FormValue("time"); t != "" {
		if v, err := strconv.ParseFloat(t, 64); err == nil {
			ts = v
		}
	}

	if fail != nil && fail.MatchString(query) {
		writeError(w, http.StatusUnprocessableEntity, "execution", "query failed on purpose")
		return
	}
	if query == "vector(1)" {
		writeVector(w, []result{{Metric: map[string]string{}, Value: [2]any{ts, "1"}}})
		return
	}

	pick, metric, ok := s.match(query)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad_data", fmt.Sprintf("fake Prometheus does not understand %q", query))
		return
	}
	var out []result
	for _, se := range s.series {
		if se.Metric != metric {
			continue
		}
		v, present := pick(se)
		if !present {
			continue
		}
		out = append(out, result{Metric: se.Labels, Value: [2]any{ts, strconv.FormatFloat(v, 'f', -1, 64)}})
	}
	writeVector(w, out)
}

func (s *Server) match(query string) (func(Series) (float64, bool), string, bool) {
	if m := simpleRe.FindStringSubmatch(query); m != nil {
		fn, metric, rng := m[1], m[2], m[3]
		switch {
		case fn == "avg_over_time":
			return func(se Series) (float64, bool) { return se.Avg, true }, metric, true
		case rng == s.Window:
			return func(se Series) (float64, bool) { return se.Max, true }, metric, true
		default:
			return func(se Series) (float64, bool) { return se.RecentMax, true }, metric, true
		}
	}
	if m := quantileRe.FindStringSubmatch(query); m != nil {
		return func(se Series) (float64, bool) { return se.P95, true }, m[1], true
	}
	if m := lastRe.FindStringSubmatch(query); m != nil {
		return func(se Series) (float64, bool) {
			if se.LastActive.IsZero() {
				return 0, false
			}
			return float64(se.LastActive.Unix()), true
		}, m[1], true
	}
	if m := countRe.FindStringSubmatch(query); m != nil {
		return func(se Series) (float64, bool) { return se.Samples, se.Samples > 0 }, m[1], true
	}
	return nil, "", false
}

func writeVector(w http.ResponseWriter, results []result) {
	if results == nil {
		results = []result{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status": "success",
		"data":   map[string]any{"resultType": "vector", "result": results},
	})
}

func writeError(w http.ResponseWriter, code int, errType, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "error", "errorType": errType, "error": msg})
}
