package checks_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/require"

	"github.com/cloudflare/pint/internal/checks"
	"github.com/cloudflare/pint/internal/discovery"
	"github.com/cloudflare/pint/internal/parser"
	"github.com/cloudflare/pint/internal/promapi"
)

func TestParseSeverity(t *testing.T) {
	type testCaseT struct {
		input       string
		output      string
		shouldError bool
	}

	testCases := []testCaseT{
		{"xxx", "", true},
		{"Bug", "", true},
		{"fatal", "Fatal", false},
		{"bug", "Bug", false},
		{"info", "Information", false},
		{"warning", "Warning", false},
	}

	for _, tc := range testCases {
		t.Run(tc.input, func(t *testing.T) {
			sev, err := checks.ParseSeverity(tc.input)
			hadError := err != nil

			if hadError != tc.shouldError {
				t.Fatalf("checks.ParseSeverity() returned err=%v, expected=%v", err, tc.shouldError)
			}

			if hadError {
				return
			}

			if sev.String() != tc.output {
				t.Fatalf("checks.ParseSeverity() returned severity=%q, expected=%q", sev, tc.output)
			}
		})
	}
}

func simpleProm(name, uri string, timeout time.Duration, required bool) *promapi.FailoverGroup {
	return promapi.NewFailoverGroup(
		name,
		[]*promapi.Prometheus{
			promapi.NewPrometheus(name, uri, timeout),
		},
		required,
	)
}

type newCheckFn func(string) checks.RuleChecker

type problemsFn func(string) []checks.Problem

type checkTest struct {
	description string
	content     string
	checker     newCheckFn
	entries     []discovery.Entry
	problems    problemsFn
	mocks       []*prometheusMock
}

func runTests(t *testing.T, testCases []checkTest) {
	ctx := context.Background()
	for _, tc := range testCases {
		// original test
		t.Run(tc.description, func(t *testing.T) {
			var uri string
			if len(tc.mocks) > 0 {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					for i := range tc.mocks {
						if tc.mocks[i].maybeApply(w, r) {
							return
						}
					}
					t.Errorf("no matching response for %s request", r.URL.Path)
					t.FailNow()
				}))
				defer srv.Close()
				uri = srv.URL
			}

			entries, err := parseContent(tc.content)
			require.NoError(t, err, "cannot parse rule content")
			for _, entry := range entries {
				problems := tc.checker(uri).Check(ctx, entry.Rule, tc.entries)
				require.Equal(t, tc.problems(uri), problems)
			}

			// verify that all mocks were used
			for _, mock := range tc.mocks {
				require.True(t, mock.wasUsed(), "unused mock in %s: %s", tc.description, mock.conds)
			}
		})

		// broken rules to test check against rules with syntax error
		entries, err := parseContent(`
- alert: foo
  expr: 'foo{}{} > 0'
  annotations:
    summary: '{{ $labels.job }} is incorrect'

- record: foo
  expr: 'foo{}{}'
`)
		require.NoError(t, err, "cannot parse rule content")
		t.Run(tc.description+" (bogus rules)", func(t *testing.T) {
			for _, entry := range entries {
				_ = tc.checker("").Check(ctx, entry.Rule, tc.entries)
			}
		})
	}
}

func parseContent(content string) (entries []discovery.Entry, err error) {
	p := parser.NewParser()
	rules, err := p.Parse([]byte(content))
	if err != nil {
		return nil, err
	}

	for _, rule := range rules {
		entries = append(entries, discovery.Entry{
			Path:          "fake.yml",
			ModifiedLines: rule.Lines(),
			Rule:          rule,
		})
	}

	return entries, nil
}

func mustParseContent(content string) (entries []discovery.Entry) {
	entries, err := parseContent(content)
	if err != nil {
		panic(err)
	}
	return entries
}

func noProblems(uri string) []checks.Problem {
	return nil
}

type requestCondition interface {
	isMatch(*http.Request) bool
}

type responseWriter interface {
	respond(w http.ResponseWriter)
}

type prometheusMock struct {
	conds []requestCondition
	resp  responseWriter
	used  bool
	mu    sync.Mutex
}

func (pm *prometheusMock) maybeApply(w http.ResponseWriter, r *http.Request) bool {
	for _, cond := range pm.conds {
		if !cond.isMatch(r) {
			return false
		}
	}
	pm.markUsed()
	pm.resp.respond(w)
	return true
}

func (pm *prometheusMock) markUsed() {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.used = true
}

func (pm *prometheusMock) wasUsed() bool {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	return pm.used
}

type requestPathCond struct {
	path string
}

func (rpc requestPathCond) isMatch(r *http.Request) bool {
	return r.URL.Path == rpc.path
}

type formCond struct {
	key   string
	value string
}

func (fc formCond) isMatch(r *http.Request) bool {
	err := r.ParseForm()
	if err != nil {
		return false
	}
	return r.Form.Get(fc.key) == fc.value
}

var (
	requireConfigPath     = requestPathCond{path: "/api/v1/status/config"}
	requireQueryPath      = requestPathCond{path: "/api/v1/query"}
	requireRangeQueryPath = requestPathCond{path: "/api/v1/query_range"}
)

type promError struct {
	code      int
	errorType v1.ErrorType
	err       string
}

func (pe promError) respond(w http.ResponseWriter) {
	w.WriteHeader(pe.code)
	w.Header().Set("Content-Type", "application/json")
	perr := struct {
		Status    string       `json:"status"`
		ErrorType v1.ErrorType `json:"errorType"`
		Error     string       `json:"error"`
	}{
		Status:    "error",
		ErrorType: pe.errorType,
		Error:     pe.err,
	}
	d, err := json.MarshalIndent(perr, "", "  ")
	if err != nil {
		panic(err)
	}
	_, _ = w.Write(d)
}

type vectorResponse struct {
	samples model.Vector
}

func (vr vectorResponse) respond(w http.ResponseWriter) {
	w.WriteHeader(200)
	w.Header().Set("Content-Type", "application/json")
	result := struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string       `json:"resultType"`
			Result     model.Vector `json:"result"`
		} `json:"data"`
	}{
		Status: "success",
		Data: struct {
			ResultType string       `json:"resultType"`
			Result     model.Vector `json:"result"`
		}{
			ResultType: "vector",
			Result:     vr.samples,
		},
	}
	d, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		panic(err)
	}
	_, _ = w.Write(d)
}

type matrixResponse struct {
	samples []*model.SampleStream
}

func (mr matrixResponse) respond(w http.ResponseWriter) {
	w.WriteHeader(200)
	w.Header().Set("Content-Type", "application/json")
	result := struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string                `json:"resultType"`
			Result     []*model.SampleStream `json:"result"`
		} `json:"data"`
	}{
		Status: "success",
		Data: struct {
			ResultType string                `json:"resultType"`
			Result     []*model.SampleStream `json:"result"`
		}{
			ResultType: "matrix",
			Result:     mr.samples,
		},
	}
	d, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		panic(err)
	}
	_, _ = w.Write(d)
}

type configResponse struct {
	yaml string
}

func (cr configResponse) respond(w http.ResponseWriter) {
	w.WriteHeader(200)
	w.Header().Set("Content-Type", "application/json")
	result := struct {
		Status string          `json:"status"`
		Data   v1.ConfigResult `json:"data"`
	}{
		Status: "success",
		Data:   v1.ConfigResult{YAML: cr.yaml},
	}
	d, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		panic(err)
	}
	_, _ = w.Write(d)
}

type sleepResponse struct {
	sleep time.Duration
}

func (sr sleepResponse) respond(w http.ResponseWriter) {
	time.Sleep(sr.sleep)
}

var (
	respondWithBadData = func() responseWriter {
		return promError{code: 400, errorType: v1.ErrBadData, err: "bad input data"}
	}
	respondWithInternalError = func() responseWriter {
		return promError{code: 500, errorType: v1.ErrServer, err: "internal error"}
	}
	respondWithEmptyVector = func() responseWriter {
		return vectorResponse{samples: model.Vector{}}
	}
	respondWithEmptyMatrix = func() responseWriter {
		return matrixResponse{samples: []*model.SampleStream{}}
	}
	respondWithSingleInstantVector = func() responseWriter {
		return vectorResponse{
			samples: []*model.Sample{generateSample(map[string]string{})},
		}
	}
	respondWithSingleRangeVector1W = func() responseWriter {
		return matrixResponse{
			samples: []*model.SampleStream{
				generateSampleStream(
					map[string]string{},
					time.Now().Add(time.Hour*24*-7),
					time.Now(),
					time.Minute*5,
				),
			},
		}
	}
)

func generateSample(labels map[string]string) *model.Sample {
	metric := model.Metric{}
	for k, v := range labels {
		metric[model.LabelName(k)] = model.LabelValue(v)
	}
	return &model.Sample{
		Metric:    metric,
		Value:     model.SampleValue(1),
		Timestamp: model.TimeFromUnix(time.Now().Unix()),
	}
}

func generateSampleStream(labels map[string]string, from, until time.Time, step time.Duration) (s *model.SampleStream) {
	metric := model.Metric{}
	for k, v := range labels {
		metric[model.LabelName(k)] = model.LabelValue(v)
	}
	s = &model.SampleStream{
		Metric: metric,
	}
	for from.Before(until) {
		s.Values = append(s.Values, model.SamplePair{
			Timestamp: model.TimeFromUnix(from.Unix()),
			Value:     1,
		})
		from = from.Add(step)
	}
	return
}

func checkErrorBadData(name, uri, err string) string {
	return fmt.Sprintf(`prometheus %q at %s failed with: %s`, name, uri, err)
}

func checkErrorUnableToRun(c, name, uri, err string) string {
	return fmt.Sprintf(`cound't run %q checks due to prometheus %q at %s connection error: %s`, c, name, uri, err)
}
