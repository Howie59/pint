package promapi_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"

	"github.com/cloudflare/pint/internal/promapi"
)

func TestQuery(t *testing.T) {
	done := sync.Map{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		err := r.ParseForm()
		if err != nil {
			t.Fatal(err)
		}
		query := r.Form.Get("query")

		switch query {
		case "empty":
			w.WriteHeader(200)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"status":"success",
				"data":{
					"resultType":"vector",
					"result":[]
				}
			}`))
		case "single_result":
			w.WriteHeader(200)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"status":"success",
				"data":{
					"resultType":"vector",
					"result":[{"metric":{},"value":[1614859502.068,"1"]}]
				}
			}`))
		case "three_results":
			w.WriteHeader(200)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"status":"success",
				"data":{
					"resultType":"vector",
					"result":[
						{"metric":{"instance": "1"},"value":[1614859502.068,"1"]},
						{"metric":{"instance": "2"},"value":[1614859502.168,"2"]},
						{"metric":{"instance": "3"},"value":[1614859503.000,"3"]}
					]
				}
			}`))
		case "once":
			if _, wasDone := done.Load(r.URL.Path); wasDone {
				w.WriteHeader(500)
				_, _ = w.Write([]byte("query already requested\n"))
				return
			}
			w.WriteHeader(200)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"status":"success",
				"data":{
					"resultType":"vector",
					"result":[{"metric":{},"value":[1614859502.068,"1"]}]
				}
			}`))
			done.Store(r.URL.Path, true)
		case "matrix":
			w.WriteHeader(200)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"status":"success",
				"data":{
					"resultType":"matrix",
					"result":[]
				}
			}`))
		case "timeout":
			w.WriteHeader(200)
			w.Header().Set("Content-Type", "application/json")
			time.Sleep(time.Second)
			_, _ = w.Write([]byte(`{
				"status":"success",
				"data":{
					"resultType":"vector",
					"result":[]
				}
			}`))
		default:
			w.WriteHeader(400)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"status":"error",
				"errorType":"bad_data",
				"error":"unhandled query"
			}`))
		}
	}))
	defer srv.Close()

	type testCaseT struct {
		query   string
		timeout time.Duration
		result  promapi.QueryResult
		err     string
		runs    int
	}

	testCases := []testCaseT{
		{
			query:   "empty",
			timeout: time.Second,
			result: promapi.QueryResult{
				URI:    srv.URL,
				Series: model.Vector{},
			},
			runs: 5,
		},
		{
			query:   "single_result",
			timeout: time.Second,
			result: promapi.QueryResult{
				URI: srv.URL,
				Series: model.Vector{
					&model.Sample{
						Metric:    model.Metric{},
						Value:     model.SampleValue(1),
						Timestamp: model.Time(1614859502068),
					},
				},
			},
			runs: 5,
		},
		{
			query:   "three_results",
			timeout: time.Second,
			result: promapi.QueryResult{
				URI: srv.URL,
				Series: model.Vector{
					&model.Sample{
						Metric:    model.Metric{"instance": "1"},
						Value:     model.SampleValue(1),
						Timestamp: model.Time(1614859502068),
					},
					&model.Sample{
						Metric:    model.Metric{"instance": "2"},
						Value:     model.SampleValue(2),
						Timestamp: model.Time(1614859502168),
					},
					&model.Sample{
						Metric:    model.Metric{"instance": "3"},
						Value:     model.SampleValue(3),
						Timestamp: model.Time(1614859503000),
					},
				},
			},
			runs: 5,
		},
		{
			query:   "error",
			timeout: time.Second,
			err:     "bad_data: unhandled query",
			runs:    5,
		},
		{
			query:   "matrix",
			timeout: time.Second,
			err:     "unknown result type: matrix",
			runs:    5,
		},
		{
			query:   "timeout",
			timeout: time.Millisecond * 20,
			err:     fmt.Sprintf(`Post "%s/api/v1/query": context deadline exceeded`, srv.URL),
			runs:    5,
		},
		{
			query:   "once",
			timeout: time.Second,
			result: promapi.QueryResult{
				URI: srv.URL,
				Series: model.Vector{
					&model.Sample{
						Metric:    model.Metric{},
						Value:     model.SampleValue(1),
						Timestamp: model.Time(1614859502068),
					},
				},
			},
			runs: 5,
		},
		// repeat once to ensure it errors
		{
			query:   "once",
			timeout: time.Second,
			err:     "server_error: server error: 500",
			runs:    5,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.query, func(t *testing.T) {
			assert := assert.New(t)

			prom := promapi.NewPrometheus("test", srv.URL, tc.timeout)

			wg := sync.WaitGroup{}
			wg.Add(tc.runs)
			for i := 1; i <= tc.runs; i++ {
				go func() {
					qr, err := prom.Query(context.Background(), tc.query)
					if tc.err != "" {
						assert.EqualError(err, tc.err, tc)
					} else {
						assert.NoError(err)
					}
					if qr != nil {
						assert.Equal(tc.result.URI, qr.URI)
						assert.Equal(tc.result.Series, qr.Series)
					}
					wg.Done()
				}()
			}
			wg.Wait()
		})
	}
}
