package logql

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/grafana/loki/v3/pkg/logqlmodel/metadata"
	"github.com/grafana/loki/v3/pkg/querier/plan"
	"github.com/grafana/loki/v3/pkg/querier/queryrange/queryrangebase/definitions"

	"github.com/go-kit/log"
	"github.com/grafana/dskit/user"
	json "github.com/json-iterator/go"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"
	promql_parser "github.com/prometheus/prometheus/promql/parser"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/grafana/loki/v3/pkg/iter"
	"github.com/grafana/loki/v3/pkg/logproto"
	"github.com/grafana/loki/v3/pkg/logql/syntax"
	"github.com/grafana/loki/v3/pkg/logqlmodel"
	"github.com/grafana/loki/v3/pkg/logqlmodel/stats"
	"github.com/grafana/loki/v3/pkg/util"
	"github.com/grafana/loki/v3/pkg/util/constants"
	"github.com/grafana/loki/v3/pkg/util/httpreq"
)

var (
	testSize        = int64(300)
	ErrMock         = errors.New("error")
	ErrMockMultiple = util.MultiError{ErrMock, ErrMock}
)

func TestEngine_checkIntervalLimit(t *testing.T) {
	q := &query{}
	for _, tc := range []struct {
		query  string
		expErr string
	}{
		{query: `rate({app="foo"} [1m])`, expErr: ""},
		{query: `rate({app="foo"} [10m])`, expErr: ""},
		{query: `max(rate({app="foo"} [5m])) - max(rate({app="bar"} [10m]))`, expErr: ""},
		{query: `rate({app="foo"} [5m]) - rate({app="bar"} [15m])`, expErr: "[15m] > [10m]"},
		{query: `rate({app="foo"} [1h])`, expErr: "[1h] > [10m]"},
		{query: `sum(rate({app="foo"} [1h]))`, expErr: "[1h] > [10m]"},
		{query: `sum_over_time({app="foo"} |= "foo" | json | unwrap bar [1h])`, expErr: "[1h] > [10m]"},
		{query: `variants(rate({app="foo"}[5m])) of ({app="foo"}[5m])`, expErr: ""},
		{query: `variants(rate({app="foo"}[1h])) of ({app="foo"}[1h])`, expErr: "[1h] > [10m]"},
	} {
		for _, downstream := range []bool{true, false} {
			t.Run(fmt.Sprintf("%v/downstream=%v", tc.query, downstream), func(t *testing.T) {
				expr := syntax.MustParseExpr(tc.query).(syntax.SampleExpr)
				if downstream {
					// Simulate downstream expression
					expr = &ConcatSampleExpr{
						DownstreamSampleExpr: DownstreamSampleExpr{
							shard:      nil,
							SampleExpr: expr,
						},
						next: nil,
					}
				}
				err := q.checkIntervalLimit(expr, 10*time.Minute)
				if tc.expErr != "" {
					require.ErrorContains(t, err, tc.expErr)
				} else {
					require.NoError(t, err)
				}
			})
		}
	}
}

func TestEngine_LogsRateUnwrap(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		qs        string
		ts        time.Time
		direction logproto.Direction
		limit     uint32

		// an array of data per params will be returned by the querier.
		// This is to cover logql that requires multiple queries.
		data   interface{}
		params interface{}

		expected interface{}
	}{
		{
			`rate({app="foo"} | unwrap foo [30s])`,
			time.Unix(60, 0),
			logproto.FORWARD,
			10,
			// create a stream {app="foo"} with 300 samples starting at 46s and ending at 345s with a constant value of 1
			[][]logproto.Series{
				// 30s range the lower bound of the range is not inclusive only 15 samples will make it 60 included
				{newSeries(testSize, offset(46, constantValue(1)), `{app="foo"}`)},
			},
			[]SelectSampleParams{
				{
					&logproto.SampleQueryRequest{
						Start:    time.Unix(30, 0),
						End:      time.Unix(60, 0),
						Selector: `rate({app="foo"} | unwrap foo[30s])`,
						Plan: &plan.QueryPlan{
							AST: syntax.MustParseExpr(`rate({app="foo"} | unwrap foo[30s])`),
						},
					},
				},
			},
			// there are 15 samples (from 47 to 61) matched from the generated series
			// SUM(n=47, 61, 1) = 15
			// 15 / 30 = 0.5
			promql.Vector{promql.Sample{T: 60 * 1000, F: 0.5, Metric: labels.FromStrings("app", "foo")}},
		},
		{
			`rate({app="foo"} | unwrap foo [30s])`,
			time.Unix(60, 0),
			logproto.FORWARD,
			10,
			// create a stream {app="foo"} with 300 samples starting at 46s and ending at 345s with an increasing value by 1
			[][]logproto.Series{
				// 30s range the lower bound of the range is not inclusive only 15 samples will make it 60 included
				{newSeries(testSize, offset(46, incValue(1)), `{app="foo"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{
					Start:    time.Unix(30, 0),
					End:      time.Unix(60, 0),
					Selector: `rate({app="foo"} | unwrap foo[30s])`,
					Plan: &plan.QueryPlan{
						AST: syntax.MustParseExpr(`rate({app="foo"} | unwrap foo[30s])`),
					},
				}},
			},
			// there are 15 samples (from 47 to 61) matched from the generated series
			// SUM(n=47, 61, n) = (47+48+...+61) = 810
			// 810 / 30 = 27
			promql.Vector{promql.Sample{T: 60 * 1000, F: 27, Metric: labels.FromStrings("app", "foo")}},
		},
		{
			`rate_counter({app="foo"} | unwrap foo [30s])`,
			time.Unix(60, 0),
			logproto.FORWARD,
			10,
			// create a stream {app="foo"} with 300 samples starting at 46s and ending at 345s with a constant value of 1
			[][]logproto.Series{
				// 30s range the lower bound of the range is not inclusive only 15 samples will make it 60 included
				{newSeries(testSize, offset(46, constantValue(1)), `{app="foo"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{
					Start:    time.Unix(30, 0),
					End:      time.Unix(60, 0),
					Selector: `rate_counter({app="foo"} | unwrap foo[30s])`,
					Plan: &plan.QueryPlan{
						AST: syntax.MustParseExpr(`rate_counter({app="foo"} | unwrap foo[30s])`),
					},
				}},
			},
			// there are 15 samples (from 47 to 61) matched from the generated series
			// (1 - 1) / 30 = 0
			promql.Vector{promql.Sample{T: 60 * 1000, F: 0, Metric: labels.FromStrings("app", "foo")}},
		},
		{
			`rate_counter({app="foo"} | unwrap foo [30s])`,
			time.Unix(60, 0),
			logproto.FORWARD,
			10,
			// create a stream {app="foo"} with 300 samples starting at 46s and ending at 345s with an increasing value by 1
			[][]logproto.Series{
				// 30s range the lower bound of the range is not inclusive only 15 samples will make it 60 included
				{newSeries(testSize, offset(46, incValue(1)), `{app="foo"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(30, 0), End: time.Unix(60, 0), Selector: `rate_counter({app="foo"} | unwrap foo[30s])`}},
			},
			// there are 15 samples (from 47 to 61) matched from the generated series
			// (61 - 47) / 30 = 0.4666
			promql.Vector{promql.Sample{T: 60 * 1000, F: 0.46666766666666665, Metric: labels.FromStrings("app", "foo")}},
		},
	} {
		t.Run(fmt.Sprintf("%s %s", test.qs, test.direction), func(t *testing.T) {
			t.Parallel()

			eng := NewEngine(EngineOpts{}, newQuerierRecorder(t, test.data, test.params), NoLimits, log.NewNopLogger())
			params, err := NewLiteralParams(test.qs, test.ts, test.ts, 0, 0, test.direction, test.limit, nil, nil)
			require.NoError(t, err)
			q := eng.Query(params)
			res, err := q.Exec(user.InjectOrgID(context.Background(), "fake"))
			if expectedError, ok := test.expected.(error); ok {
				assert.Equal(t, expectedError.Error(), err.Error())
			} else {
				if err != nil {
					t.Fatal(err)
				}
				assert.Equal(t, test.expected, res.Data)
			}
		})
	}
}

func TestEngine_InstantQuery(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		qs        string
		ts        time.Time
		direction logproto.Direction
		limit     uint32

		// an array of data per params will be returned by the querier.
		// This is to cover logql that requires multiple queries.
		data   interface{}
		params interface{}

		expected interface{}
	}{
		{
			`rate({app="foo"} |~".+bar" [1m])`, time.Unix(60, 0), logproto.BACKWARD, 10,
			[][]logproto.Series{
				{newSeries(testSize, identity, `{app="foo"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `rate({app="foo"}|~".+bar"[1m])`}},
			},
			promql.Vector{promql.Sample{T: 60 * 1000, F: 1, Metric: labels.FromStrings("app", "foo")}},
		},
		{
			`rate({app="foo"}[30s])`, time.Unix(60, 0), logproto.FORWARD, 10,
			[][]logproto.Series{
				// 30s range the lower bound of the range is not inclusive only 15 samples will make it 60 included
				{newSeries(testSize, offset(46, identity), `{app="foo"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(30, 0), End: time.Unix(60, 0), Selector: `rate({app="foo"}[30s])`}},
			},
			promql.Vector{promql.Sample{T: 60 * 1000, F: 0.5, Metric: labels.FromStrings("app", "foo")}},
		},
		{
			`rate({app="foo"} | unwrap foo [30s])`, time.Unix(60, 0), logproto.FORWARD, 10,
			[][]logproto.Series{
				// 30s range the lower bound of the range is not inclusive only 15 samples will make it 60 included
				{newSeries(testSize, offset(46, constantValue(2)), `{app="foo"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(30, 0), End: time.Unix(60, 0), Selector: `rate({app="foo"} | unwrap foo[30s])`}},
			},
			// SUM(n=46, 61, 2) = 30
			// 30 / 30 = 1
			promql.Vector{promql.Sample{T: 60 * 1000, F: 1.0, Metric: labels.FromStrings("app", "foo")}},
		},
		{
			`count_over_time({app="foo"} |~".+bar" [1m])`, time.Unix(60, 0), logproto.BACKWARD, 10,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`)}, // 10 , 20 , 30 .. 60 = 6 total
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `count_over_time({app="foo"}|~".+bar"[1m])`}},
			},
			promql.Vector{promql.Sample{T: 60 * 1000, F: 6, Metric: labels.FromStrings("app", "foo")}},
		},
		{
			`first_over_time({app="foo"} |~".+bar" | unwrap foo [1m])`, time.Unix(60, 0), logproto.BACKWARD, 10,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`)}, // 10 , 20 , 30 .. 60 = 6 total
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `first_over_time({app="foo"}|~".+bar"| unwrap foo [1m])`}},
			},
			promql.Vector{promql.Sample{T: 60 * 1000, F: 1, Metric: labels.FromStrings("app", "foo")}},
		},
		{
			`count_over_time({app="foo"} |~".+bar" [1m] offset 30s)`, time.Unix(90, 0), logproto.BACKWARD, 10,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`)}, // 10 , 20 , 30 .. 60 = 6 total
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `count_over_time({app="foo"}|~".+bar"[1m] offset 30s)`}},
			},
			promql.Vector{promql.Sample{T: 90 * 1000, F: 6, Metric: labels.FromStrings("app", "foo")}},
		},
		{
			`count_over_time(({app="foo"} |~".+bar")[5m])`, time.Unix(5*60, 0), logproto.BACKWARD, 10,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`)}, // 10 , 20 , 30 .. 300 = 30 total
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(5*60, 0), Selector: `count_over_time({app="foo"}|~".+bar"[5m])`}},
			},
			promql.Vector{promql.Sample{T: 5 * 60 * 1000, F: 30, Metric: labels.FromStrings("app", "foo")}},
		},
		{
			`absent_over_time(({app="foo"} |~".+bar")[5m])`, time.Unix(5*60, 0), logproto.BACKWARD, 10,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`)}, // 10 , 20 , 30 .. 300 = 30 total
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(5*60, 0), Selector: `absent_over_time({app="foo"}|~".+bar"[5m])`}},
			},
			promql.Vector{},
		},
		{
			`absent_over_time(({app="foo"} |~".+bar")[5m])`, time.Unix(5*60, 0), logproto.BACKWARD, 10,
			[][]logproto.Series{},
			[]SelectSampleParams{},
			promql.Vector{promql.Sample{T: 5 * 60 * 1000, F: 1, Metric: labels.FromStrings("app", "foo")}},
		},
		{
			`avg(count_over_time({app=~"foo|bar"} |~".+bar" [1m]))`, time.Unix(60, 0), logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(10, identity), `{app="foo"}`),
					newSeries(testSize, factor(10, identity), `{app="bar"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `count_over_time({app=~"foo|bar"}|~".+bar"[1m])`}},
			},
			promql.Vector{
				promql.Sample{T: 60 * 1000, F: 6, Metric: labels.EmptyLabels()},
			},
		},
		{
			`min(rate({app=~"foo|bar"} |~".+bar" [1m]))`, time.Unix(60, 0), logproto.FORWARD, 100,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`), newSeries(testSize, factor(10, identity), `{app="bar"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `rate({app=~"foo|bar"}|~".+bar"[1m])`}},
			},
			promql.Vector{
				promql.Sample{T: 60 * 1000, F: 0.1, Metric: labels.EmptyLabels()},
			},
		},
		{
			`max by (app) (rate({app=~"foo|bar"} |~".+bar" [1m]))`, time.Unix(60, 0), logproto.FORWARD, 100,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`), newSeries(testSize, factor(5, identity), `{app="bar"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `rate({app=~"foo|bar"}|~".+bar"[1m])`}},
			},
			promql.Vector{
				promql.Sample{T: 60 * 1000, F: 0.2, Metric: labels.FromStrings("app", "bar")},
				promql.Sample{T: 60 * 1000, F: 0.1, Metric: labels.FromStrings("app", "foo")},
			},
		},
		{
			`max(rate({app=~"foo|bar"} |~".+bar" [1m]))`, time.Unix(60, 0), logproto.FORWARD, 100,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`), newSeries(testSize, factor(5, identity), `{app="bar"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `rate({app=~"foo|bar"}|~".+bar"[1m])`}},
			},
			promql.Vector{
				promql.Sample{T: 60 * 1000, F: 0.2, Metric: labels.EmptyLabels()},
			},
		},
		{
			`sum(rate({app=~"foo|bar"} |~".+bar" [1m]))`, time.Unix(60, 0), logproto.FORWARD, 100,
			[][]logproto.Series{
				{newSeries(testSize, factor(5, identity), `{app="foo"}`), newSeries(testSize, factor(5, identity), `{app="bar"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum(rate({app=~"foo|bar"} |~".+bar" [1m]))`}},
			},
			promql.Vector{
				promql.Sample{T: 60 * 1000, F: 0.4, Metric: labels.EmptyLabels()},
			},
		},
		{
			`sum(count_over_time({app=~"foo|bar"} |~".+bar" [1m])) by (app)`, time.Unix(60, 0), logproto.FORWARD, 100,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`), newSeries(testSize, factor(10, identity), `{app="bar"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum by (app)(count_over_time({app=~"foo|bar"} |~".+bar" [1m]))`}},
			},
			promql.Vector{
				promql.Sample{T: 60 * 1000, F: 6, Metric: labels.FromStrings("app", "bar")},
				promql.Sample{T: 60 * 1000, F: 6, Metric: labels.FromStrings("app", "foo")},
			},
		},
		{
			`sum(count_over_time({app=~"foo|bar"} |~".+bar" [1m])) by (namespace,app)`, time.Unix(60, 0), logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(10, identity), `{app="foo", namespace="a"}`),
					newSeries(testSize, factor(10, identity), `{app="bar", namespace="b"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum by (namespace,app) (count_over_time({app=~"foo|bar"} |~".+bar" [1m])) `}},
			},
			promql.Vector{
				promql.Sample{
					T: 60 * 1000,
					F: 6,
					Metric: labels.FromStrings("app", "bar",
						"namespace", "b",
					),
				},
				promql.Sample{
					T: 60 * 1000,
					F: 6,
					Metric: labels.FromStrings("app", "foo",
						"namespace", "a",
					),
				},
			},
		},
		{
			`sum(count_over_time({app=~"foo|bar"} |~".+bar" [1m] offset 30s)) by (namespace,app)`, time.Unix(90, 0), logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(10, identity), `{app="foo", namespace="a"}`),
					newSeries(testSize, factor(10, identity), `{app="bar", namespace="b"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum by (namespace,app) (count_over_time({app=~"foo|bar"} |~".+bar" [1m] offset 30s)) `}},
			},
			promql.Vector{
				promql.Sample{
					T: 90 * 1000, F: 6,
					Metric: labels.FromStrings("app", "bar",
						"namespace", "b",
					),
				},
				promql.Sample{
					T: 90 * 1000, F: 6,
					Metric: labels.FromStrings("app", "foo",
						"namespace", "a",
					),
				},
			},
		},
		{
			`label_replace(
				sum(count_over_time({app=~"foo|bar"} |~".+bar" [1m])) by (namespace,app),
				"new",
				"$1",
				"app",
				"f(.*)"
				)`, time.Unix(60, 0), logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(10, identity), `{app="foo", namespace="a"}`),
					newSeries(testSize, factor(10, identity), `{app="bar", namespace="b"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum by (namespace,app) (count_over_time({app=~"foo|bar"} |~".+bar" [1m])) `}},
			},
			promql.Vector{
				promql.Sample{
					T: 60 * 1000, F: 6,
					Metric: labels.FromStrings("app", "bar",
						"namespace", "b",
					),
				},
				promql.Sample{
					T: 60 * 1000, F: 6,
					Metric: labels.FromStrings("app", "foo",
						"namespace", "a",
						"new", "oo",
					),
				},
			},
		},
		{
			`count(count_over_time({app=~"foo|bar"} |~".+bar" [1m])) without (app)`, time.Unix(60, 0), logproto.FORWARD, 100,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`), newSeries(testSize, factor(10, identity), `{app="bar"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `count_over_time({app=~"foo|bar"}|~".+bar"[1m])`}},
			},
			promql.Vector{
				{T: 60 * 1000, F: 2, Metric: labels.EmptyLabels()},
			},
		},
		{
			`stdvar without (app) (count_over_time(({app=~"foo|bar"} |~".+bar")[1m])) `, time.Unix(60, 0), logproto.FORWARD, 100,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`), newSeries(testSize, factor(5, identity), `{app="bar"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `count_over_time({app=~"foo|bar"}|~".+bar"[1m])`}},
			},
			promql.Vector{
				{T: 60 * 1000, F: 9, Metric: labels.EmptyLabels()},
			},
		},
		{
			`stddev(count_over_time(({app=~"foo|bar"} |~".+bar")[1m])) `, time.Unix(60, 0), logproto.FORWARD, 100,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`), newSeries(testSize, factor(2, identity), `{app="bar"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `count_over_time({app=~"foo|bar"}|~".+bar"[1m])`}},
			},
			promql.Vector{
				{T: 60 * 1000, F: 12, Metric: labels.EmptyLabels()},
			},
		},
		{
			`rate(({app=~"foo|bar"} |~".+bar")[1m])`, time.Unix(60, 0), logproto.FORWARD, 100,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`), newSeries(testSize, offset(46, identity), `{app="bar"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `rate({app=~"foo|bar"}|~".+bar"[1m])`}},
			},
			promql.Vector{
				{T: 60 * 1000, F: 0.25, Metric: labels.FromStrings("app", "bar")},
				{T: 60 * 1000, F: 0.1, Metric: labels.FromStrings("app", "foo")},
			},
		},
		{
			`topk(2,rate(({app=~"foo|bar"} |~".+bar")[1m]))`, time.Unix(60, 0), logproto.FORWARD, 100,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`), newSeries(testSize, offset(46, identity), `{app="bar"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `rate({app=~"foo|bar"}|~".+bar"[1m])`}},
			},
			promql.Vector{
				{T: 60 * 1000, F: 0.25, Metric: labels.FromStrings("app", "bar")},
				{T: 60 * 1000, F: 0.1, Metric: labels.FromStrings("app", "foo")},
			},
		},
		{
			`topk(1,rate(({app=~"foo|bar"} |~".+bar")[1m]))`, time.Unix(60, 0), logproto.FORWARD, 100,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`), newSeries(testSize, offset(46, identity), `{app="bar"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `rate({app=~"foo|bar"}|~".+bar"[1m])`}},
			},
			promql.Vector{
				{T: 60 * 1000, F: 0.25, Metric: labels.FromStrings("app", "bar")},
			},
		},

		{
			`topk(1,rate(({app=~"foo|bar"} |~".+bar")[1m])) by (app)`, time.Unix(60, 0), logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(10, identity), `{app="foo"}`), newSeries(testSize, offset(46, identity), `{app="bar"}`),
					newSeries(testSize, factor(5, identity), `{app="fuzz"}`), newSeries(testSize, identity, `{app="buzz"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `rate({app=~"foo|bar"}|~".+bar"[1m])`}},
			},
			promql.Vector{
				{T: 60 * 1000, F: 0.25, Metric: labels.FromStrings("app", "bar")},
				{T: 60 * 1000, F: 1, Metric: labels.FromStrings("app", "buzz")},
				{T: 60 * 1000, F: 0.1, Metric: labels.FromStrings("app", "foo")},
				{T: 60 * 1000, F: 0.2, Metric: labels.FromStrings("app", "fuzz")},
			},
		},
		{
			`bottomk(2,rate(({app=~"foo|bar"} |~".+bar")[1m]))`, time.Unix(60, 0), logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(10, identity), `{app="foo"}`), newSeries(testSize, offset(46, identity), `{app="bar"}`),
					newSeries(testSize, factor(5, identity), `{app="fuzz"}`), newSeries(testSize, identity, `{app="buzz"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `rate({app=~"foo|bar"}|~".+bar"[1m])`}},
			},
			promql.Vector{
				{T: 60 * 1000, F: 0.1, Metric: labels.FromStrings("app", "foo")},
				{T: 60 * 1000, F: 0.2, Metric: labels.FromStrings("app", "fuzz")},
			},
		},
		{
			`bottomk(3,rate(({app=~"foo|bar"} |~".+bar")[1m])) without (app)`, time.Unix(60, 0), logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(10, identity), `{app="foo"}`), newSeries(testSize, offset(46, identity), `{app="bar"}`),
					newSeries(testSize, factor(5, identity), `{app="fuzz"}`), newSeries(testSize, identity, `{app="buzz"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `rate({app=~"foo|bar"}|~".+bar"[1m])`}},
			},
			promql.Vector{
				{T: 60 * 1000, F: 0.25, Metric: labels.FromStrings("app", "bar")},
				{T: 60 * 1000, F: 0.1, Metric: labels.FromStrings("app", "foo")},
				{T: 60 * 1000, F: 0.2, Metric: labels.FromStrings("app", "fuzz")},
			},
		},
		{
			`bottomk(3,rate(({app=~"foo|bar"} |~".+bar")[1m])) without (app) + 1`, time.Unix(60, 0), logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(10, identity), `{app="foo"}`), newSeries(testSize, offset(46, identity), `{app="bar"}`),
					newSeries(testSize, factor(5, identity), `{app="fuzz"}`), newSeries(testSize, identity, `{app="buzz"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `rate({app=~"foo|bar"}|~".+bar"[1m])`}},
			},
			promql.Vector{
				{T: 60 * 1000, F: 1.25, Metric: labels.FromStrings("app", "bar")},
				{T: 60 * 1000, F: 1.1, Metric: labels.FromStrings("app", "foo")},
				{T: 60 * 1000, F: 1.2, Metric: labels.FromStrings("app", "fuzz")},
			},
		},
		// sort and sort_desc
		{
			`sort(rate(({app=~"foo|bar"} |~".+bar")[1m]))  + 1`, time.Unix(60, 0), logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(10, identity), `{app="foo"}`), newSeries(testSize, offset(46, identity), `{app="bar"}`),
					newSeries(testSize, factor(5, identity), `{app="fuzz"}`), newSeries(testSize, identity, `{app="buzz"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `rate({app=~"foo|bar"}|~".+bar"[1m])`}},
			},
			promql.Vector{
				{T: 60 * 1000, F: 1.1, Metric: labels.FromStrings("app", "foo")},
				{T: 60 * 1000, F: 1.2, Metric: labels.FromStrings("app", "fuzz")},
				{T: 60 * 1000, F: 1.25, Metric: labels.FromStrings("app", "bar")},
				{T: 60 * 1000, F: 2, Metric: labels.FromStrings("app", "buzz")},
			},
		},
		{
			`sort_desc(rate(({app=~"foo|bar"} |~".+bar")[1m]))  + 1`, time.Unix(60, 0), logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(10, identity), `{app="foo"}`), newSeries(testSize, offset(46, identity), `{app="bar"}`),
					newSeries(testSize, factor(5, identity), `{app="fuzz"}`), newSeries(testSize, identity, `{app="buzz"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `rate({app=~"foo|bar"}|~".+bar"[1m])`}},
			},
			promql.Vector{
				{T: 60 * 1000, F: 2, Metric: labels.FromStrings("app", "buzz")},
				{T: 60 * 1000, F: 1.25, Metric: labels.FromStrings("app", "bar")},
				{T: 60 * 1000, F: 1.2, Metric: labels.FromStrings("app", "fuzz")},
				{T: 60 * 1000, F: 1.1, Metric: labels.FromStrings("app", "foo")},
			},
		},
		{
			// healthcheck
			`1+1`, time.Unix(60, 0), logproto.FORWARD, 100,
			nil,
			nil,
			promql.Scalar{T: 60 * 1000, V: 2},
		},
		{
			// single literal
			`2`,
			time.Unix(60, 0), logproto.FORWARD, 100,
			nil,
			nil,
			promql.Scalar{T: 60 * 1000, V: 2},
		},
		{
			// vector instant
			`vector(2)`,
			time.Unix(60, 0), logproto.FORWARD, 100,
			nil,
			nil,
			promql.Vector{promql.Sample{
				T: 60 * 1000, F: 2,
				Metric: labels.EmptyLabels(),
			}},
		},
		{
			// single comparison
			`1 == 1`,
			time.Unix(60, 0), logproto.FORWARD, 100,
			nil,
			nil,
			promql.Scalar{T: 60 * 1000, V: 1},
		},
		{
			// single comparison, reduce away bool modifier between scalars
			`1 == bool 1`,
			time.Unix(60, 0), logproto.FORWARD, 100,
			nil,
			nil,
			promql.Scalar{T: 60 * 1000, V: 1},
		},
		{
			`count_over_time({app="foo"}[1m]) > 1`,
			time.Unix(60, 0),
			logproto.FORWARD,
			0,
			[][]logproto.Series{
				{newSeries(testSize, identity, `{app="foo"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `count_over_time({app="foo"}[1m])`}},
			},
			promql.Vector{
				{T: 60 * 1000, F: 60, Metric: labels.FromStrings("app", "foo")},
			},
		},
		{
			// should return same results as `count_over_time({app="foo"}[1m]) > 1`.
			// https://grafana.com/docs/loki/latest/query/#comparison-operators
			// Between a vector and a scalar, these operators are
			// applied to the value of every data sample in the vector
			`1 < count_over_time({app="foo"}[1m])`,
			time.Unix(60, 0),
			logproto.FORWARD,
			0,
			[][]logproto.Series{
				{newSeries(testSize, identity, `{app="foo"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `count_over_time({app="foo"}[1m])`}},
			},
			promql.Vector{
				{T: 60 * 1000, F: 60, Metric: labels.FromStrings("app", "foo")},
			},
		},
		{
			`count_over_time({app="foo"}[1m]) > count_over_time({app="bar"}[1m])`,
			time.Unix(60, 0),
			logproto.FORWARD,
			0,
			[][]logproto.Series{
				{newSeries(testSize, identity, `{app="foo"}`)},
				{},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `count_over_time({app="foo"}[1m])`}},
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `count_over_time({app="bar"}[1m])`}},
			},
			promql.Vector{},
		},
		{
			`count_over_time({app="foo"}[1m]) > bool count_over_time({app="bar"}[1m])`,
			time.Unix(60, 0),
			logproto.FORWARD,
			0,
			[][]logproto.Series{
				{newSeries(testSize, identity, `{app="foo"}`)},
				{},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `count_over_time({app="foo"}[1m])`}},
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `count_over_time({app="bar"}[1m])`}},
			},
			promql.Vector{},
		},
		{
			`sum without(app) (count_over_time({app="foo"}[1m])) > bool sum without(app) (count_over_time({app="bar"}[1m]))`,
			time.Unix(60, 0),
			logproto.FORWARD,
			0,
			[][]logproto.Series{
				{newSeries(testSize, identity, `{app="foo"}`)},
				{},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum without (app) (count_over_time({app="foo"}[1m]))`}},
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum without (app) (count_over_time({app="bar"}[1m]))`}},
			},
			promql.Vector{},
		},
		{
			`sum without(app) (count_over_time({app="foo"}[1m])) >= sum without(app) (count_over_time({app="bar"}[1m]))`,
			time.Unix(60, 0),
			logproto.FORWARD,
			0,
			[][]logproto.Series{
				{newSeries(testSize, identity, `{app="foo"}`)},
				{newSeries(testSize, identity, `{app="bar"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum without(app) (count_over_time({app="foo"}[1m]))`}},
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum without(app) (count_over_time({app="bar"}[1m]))`}},
			},
			promql.Vector{
				{T: 60 * 1000, F: 60, Metric: labels.EmptyLabels()},
			},
		},
		{
			`10 / 5 / 2`,
			time.Unix(60, 0),
			logproto.FORWARD,
			0,
			nil,
			nil,
			promql.Scalar{T: 60 * 1000, V: 1},
		},
		{
			`10 / (5 / 2)`,
			time.Unix(60, 0),
			logproto.FORWARD,
			0,
			nil,
			nil,
			promql.Scalar{T: 60 * 1000, V: 4},
		},
		{
			`10 / ((rate({app="foo"} |~".+bar" [1m]) /5))`, time.Unix(60, 0), logproto.BACKWARD, 10,
			[][]logproto.Series{
				{newSeries(testSize, identity, `{app="foo"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `rate({app="foo"}|~".+bar"[1m])`}},
			},
			promql.Vector{{T: 60 * 1000, F: 50, Metric: labels.FromStrings("app", "foo")}},
		},
		{
			`sum by (app) (count_over_time({app="foo"}[1m])) + sum by (app) (count_over_time({app="bar"}[1m]))`,
			time.Unix(60, 0),
			logproto.FORWARD,
			0,
			[][]logproto.Series{
				{newSeries(testSize, identity, `{app="foo"}`)},
				{newSeries(testSize, identity, `{app="bar"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum by (app) (count_over_time({app="foo"}[1m]))`}},
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum by (app) (count_over_time({app="bar"}[1m]))`}},
			},
			promql.Vector{},
		},
		{
			`sum by (app) (count_over_time({app="foo"}[1m])) + sum by (app) (count_over_time({app="foo"}[1m]))`,
			time.Unix(60, 0),
			logproto.FORWARD,
			0,
			[][]logproto.Series{
				{newSeries(testSize, identity, `{app="foo"}`)},
				{newSeries(testSize, identity, `{app="foo"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum by (app) (count_over_time({app="foo"}[1m]))`}},
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum by (app) (count_over_time({app="foo"}[1m]))`}},
			},
			promql.Vector{
				{T: 60 * 1000, F: 120, Metric: labels.FromStrings("app", "foo")},
			},
		},
		{
			`sum by (app,machine) (count_over_time({app="foo"}[1m])) + on () sum by (app) (count_over_time({app="foo"}[1m]))`,
			time.Unix(60, 0),
			logproto.FORWARD,
			0,
			[][]logproto.Series{
				{newSeries(testSize, identity, `{app="foo",machine="fuzz"}`)},
				{newSeries(testSize, identity, `{app="foo"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum by (app,machine) (count_over_time({app="foo"}[1m]))`}},
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum by (app) (count_over_time({app="foo"}[1m]))`}},
			},
			promql.Vector{
				{T: 60 * 1000, F: 120, Metric: labels.EmptyLabels()},
			},
		},
		{
			`sum by (app,machine) (count_over_time({app="foo"}[1m])) + on (app) sum by (app) (count_over_time({app="foo"}[1m]))`,
			time.Unix(60, 0),
			logproto.FORWARD,
			0,
			[][]logproto.Series{
				{newSeries(testSize, identity, `{app="foo",machine="fuzz"}`)},
				{newSeries(testSize, identity, `{app="foo"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum by (app,machine) (count_over_time({app="foo"}[1m]))`}},
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum by (app) (count_over_time({app="foo"}[1m]))`}},
			},
			promql.Vector{
				{T: 60 * 1000, F: 120, Metric: labels.FromStrings("app", "foo")},
			},
		},
		{
			`sum by (app,machine) (count_over_time({app="foo"}[1m])) > bool ignoring (machine) sum by (app) (count_over_time({app="foo"}[1m]))`,
			time.Unix(60, 0),
			logproto.FORWARD,
			0,
			[][]logproto.Series{
				{newSeries(testSize, identity, `{app="foo",machine="fuzz"}`)},
				{newSeries(testSize, identity, `{app="foo"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum by (app,machine) (count_over_time({app="foo"}[1m]))`}},
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum by (app) (count_over_time({app="foo"}[1m]))`}},
			},
			promql.Vector{
				{T: 60 * 1000, F: 0, Metric: labels.FromStrings("app", "foo")},
			},
		},
		{
			`sum by (app,machine) (count_over_time({app="foo"}[1m])) > bool ignoring (machine) sum by (app) (count_over_time({app="foo"}[1m]))`,
			time.Unix(60, 0),
			logproto.FORWARD,
			0,
			[][]logproto.Series{
				{newSeries(testSize, identity, `{app="foo",machine="fuzz"}`), newSeries(testSize, identity, `{app="foo",machine="buzz"}`)},
				{newSeries(testSize, identity, `{app="foo"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum by (app,machine) (count_over_time({app="foo"}[1m]))`}},
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum by (app) (count_over_time({app="foo"}[1m]))`}},
			},
			errors.New("multiple matches for labels: many-to-one matching must be explicit (group_left/group_right)"),
		},
		{
			`sum by (app,machine) (count_over_time({app="foo"}[1m])) > bool on () group_left sum by (app) (count_over_time({app="foo"}[1m]))`,
			time.Unix(60, 0),
			logproto.FORWARD,
			0,
			[][]logproto.Series{
				{newSeries(testSize, identity, `{app="foo",machine="fuzz"}`), newSeries(testSize, identity, `{app="foo",machine="buzz"}`)},
				{newSeries(testSize, identity, `{app="foo"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum by (app,machine) (count_over_time({app="foo"}[1m]))`}},
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum by (app) (count_over_time({app="foo"}[1m]))`}},
			},
			promql.Vector{
				{T: 60 * 1000, F: 0, Metric: labels.FromStrings("app", "foo", "machine", "buzz")},
				{T: 60 * 1000, F: 0, Metric: labels.FromStrings("app", "foo", "machine", "fuzz")},
			},
		},
		{
			`sum by (app,machine) (count_over_time({app="foo"}[1m])) > bool on () group_left () sum by (app) (count_over_time({app="foo"}[1m]))`,
			time.Unix(60, 0),
			logproto.FORWARD,
			0,
			[][]logproto.Series{
				{newSeries(testSize, identity, `{app="foo",machine="fuzz"}`), newSeries(testSize, identity, `{app="foo",machine="buzz"}`)},
				{newSeries(testSize, identity, `{app="foo"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum by (app,machine) (count_over_time({app="foo"}[1m]))`}},
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum by (app) (count_over_time({app="foo"}[1m]))`}},
			},
			promql.Vector{
				{T: 60 * 1000, F: 0, Metric: labels.FromStrings("app", "foo", "machine", "buzz")},
				{T: 60 * 1000, F: 0, Metric: labels.FromStrings("app", "foo", "machine", "fuzz")},
			},
		},
		{
			`sum by (app,machine) (count_over_time({app="foo"}[1m])) > bool on (app) group_left (pool) sum by (app,pool) (count_over_time({app="foo"}[1m]))`,
			time.Unix(60, 0),
			logproto.FORWARD,
			0,
			[][]logproto.Series{
				{newSeries(testSize, identity, `{app="foo",machine="fuzz"}`), newSeries(testSize, identity, `{app="foo",machine="buzz"}`)},
				{newSeries(testSize, identity, `{app="foo",pool="foo"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum by (app,machine) (count_over_time({app="foo"}[1m]))`}},
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum by (app,pool) (count_over_time({app="foo"}[1m]))`}},
			},
			promql.Vector{
				{T: 60 * 1000, F: 0, Metric: labels.FromStrings("app", "foo", "machine", "buzz", "pool", "foo")},
				{T: 60 * 1000, F: 0, Metric: labels.FromStrings("app", "foo", "machine", "fuzz", "pool", "foo")},
			},
		},
		{
			`sum by (app,pool) (count_over_time({app="foo"}[1m])) > bool on (app) group_right (pool) sum by (app,machine) (count_over_time({app="foo"}[1m]))`,
			time.Unix(60, 0),
			logproto.FORWARD,
			0,
			[][]logproto.Series{
				{newSeries(testSize, identity, `{app="foo",pool="foo"}`)},
				{newSeries(testSize, identity, `{app="foo",machine="fuzz"}`), newSeries(testSize, identity, `{app="foo",machine="buzz"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum by (app,pool) (count_over_time({app="foo"}[1m]))`}},
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(60, 0), Selector: `sum by (app,machine) (count_over_time({app="foo"}[1m]))`}},
			},
			promql.Vector{
				{T: 60 * 1000, F: 0, Metric: labels.FromStrings("app", "foo", "machine", "buzz", "pool", "foo")},
				{T: 60 * 1000, F: 0, Metric: labels.FromStrings("app", "foo", "machine", "fuzz", "pool", "foo")},
			},
		},
	} {
		t.Run(fmt.Sprintf("%s %s", test.qs, test.direction), func(t *testing.T) {
			eng := NewEngine(EngineOpts{}, newQuerierRecorder(t, test.data, test.params), NoLimits, log.NewNopLogger())

			params, err := NewLiteralParams(test.qs, test.ts, test.ts, 0, 0, test.direction, test.limit, nil, nil)
			require.NoError(t, err)
			q := eng.Query(params)
			res, err := q.Exec(user.InjectOrgID(context.Background(), "fake"))
			if expectedError, ok := test.expected.(error); ok {
				assert.Equal(t, expectedError.Error(), err.Error())
			} else {
				if err != nil {
					t.Fatal(err)
				}
				assert.Equal(t, test.expected, res.Data)
			}
		})
	}
}

func TestEngine_RangeQuery(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		qs        string
		start     time.Time
		end       time.Time
		step      time.Duration
		interval  time.Duration
		direction logproto.Direction
		limit     uint32

		// an array of streams per SelectParams will be returned by the querier.
		// This is to cover logql that requires multiple queries.
		data   interface{}
		params interface{}

		expected promql_parser.Value
	}{
		{
			`{app="foo"}`, time.Unix(0, 0), time.Unix(30, 0), time.Second, 0, logproto.FORWARD, 10,
			[][]logproto.Stream{
				{newStream(testSize, identity, `{app="foo"}`)},
			},
			[]SelectLogParams{
				{&logproto.QueryRequest{Direction: logproto.FORWARD, Start: time.Unix(0, 0), End: time.Unix(30, 0), Limit: 10, Selector: `{app="foo"}`}},
			},
			logqlmodel.Streams([]logproto.Stream{newStream(10, identity, `{app="foo"}`)}),
		},
		{
			`{app="food"}`, time.Unix(0, 0), time.Unix(30, 0), 0, 2 * time.Second, logproto.FORWARD, 10,
			[][]logproto.Stream{
				{newStream(testSize, identity, `{app="food"}`)},
			},
			[]SelectLogParams{
				{&logproto.QueryRequest{Direction: logproto.FORWARD, Start: time.Unix(0, 0), End: time.Unix(30, 0), Limit: 10, Selector: `{app="food"}`}},
			},
			logqlmodel.Streams([]logproto.Stream{newIntervalStream(10, 2*time.Second, identity, `{app="food"}`)}),
		},
		{
			`{app="fed"}`, time.Unix(0, 0), time.Unix(30, 0), 0, 2 * time.Second, logproto.BACKWARD, 10,
			[][]logproto.Stream{
				{newBackwardStream(testSize, identity, `{app="fed"}`)},
			},
			[]SelectLogParams{
				{&logproto.QueryRequest{Direction: logproto.BACKWARD, Start: time.Unix(0, 0), End: time.Unix(30, 0), Limit: 10, Selector: `{app="fed"}`}},
			},
			logqlmodel.Streams([]logproto.Stream{newBackwardIntervalStream(testSize, 10, 2*time.Second, identity, `{app="fed"}`)}),
		},
		{
			`{app="bar"} |= "foo" |~ ".+bar"`, time.Unix(0, 0), time.Unix(30, 0), time.Second, 0, logproto.BACKWARD, 30,
			[][]logproto.Stream{
				{newStream(testSize, identity, `{app="bar"}`)},
			},
			[]SelectLogParams{
				{&logproto.QueryRequest{Direction: logproto.BACKWARD, Start: time.Unix(0, 0), End: time.Unix(30, 0), Limit: 30, Selector: `{app="bar"}|="foo"|~".+bar"`}},
			},
			logqlmodel.Streams([]logproto.Stream{newStream(30, identity, `{app="bar"}`)}),
		},
		{
			`{app="barf"} |= "foo" |~ ".+bar"`, time.Unix(0, 0), time.Unix(30, 0), 0, 3 * time.Second, logproto.BACKWARD, 30,
			[][]logproto.Stream{
				{newBackwardStream(testSize, identity, `{app="barf"}`)},
			},
			[]SelectLogParams{
				{&logproto.QueryRequest{Direction: logproto.BACKWARD, Start: time.Unix(0, 0), End: time.Unix(30, 0), Limit: 30, Selector: `{app="barf"}|="foo"|~".+bar"`}},
			},
			logqlmodel.Streams([]logproto.Stream{newBackwardIntervalStream(testSize, 30, 3*time.Second, identity, `{app="barf"}`)}),
		},
		{
			`rate({app="foo"} |~".+bar" [1m])`, time.Unix(60, 0), time.Unix(120, 0), time.Minute, 0, logproto.BACKWARD, 10,
			[][]logproto.Series{
				{newSeries(testSize, identity, `{app="foo"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(120, 0), Selector: `rate({app="foo"}|~".+bar"[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.FromStrings("app", "foo"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 1}, {T: 120 * 1000, F: 1}},
				},
			},
		},
		{
			`rate({app="foo"}[30s])`, time.Unix(60, 0), time.Unix(120, 0), 15 * time.Second, 0, logproto.FORWARD, 10,
			[][]logproto.Series{
				{newSeries(testSize, factor(2, identity), `{app="foo"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(30, 0), End: time.Unix(120, 0), Selector: `rate({app="foo"}[30s])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.FromStrings("app", "foo"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 0.5}, {T: 75 * 1000, F: 0.5}, {T: 90 * 1000, F: 0.5}, {T: 105 * 1000, F: 0.5}, {T: 120 * 1000, F: 0.5}},
				},
			},
		},
		{
			`count_over_time({app="foo"} |~".+bar" [1m])`, time.Unix(60, 0), time.Unix(120, 0), 30 * time.Second, 0, logproto.BACKWARD, 10,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`)}, // 10 , 20 , 30 .. 60 = 6 total
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(120, 0), Selector: `count_over_time({app="foo"}|~".+bar"[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.FromStrings("app", "foo"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 6}, {T: 90 * 1000, F: 6}, {T: 120 * 1000, F: 6}},
				},
			},
		},
		{
			`count_over_time(({app="foo"} |~".+bar")[5m])`, time.Unix(5*60, 0), time.Unix(5*120, 0), 30 * time.Second, 0, logproto.BACKWARD, 10,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`)}, // 10 , 20 , 30 .. 300 = 30 total
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(5*120, 0), Selector: `count_over_time({app="foo"}|~".+bar"[5m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.FromStrings("app", "foo"),
					Floats: []promql.FPoint{
						{T: 300 * 1000, F: 30},
						{T: 330 * 1000, F: 30},
						{T: 360 * 1000, F: 30},
						{T: 390 * 1000, F: 30},
						{T: 420 * 1000, F: 30},
						{T: 450 * 1000, F: 30},
						{T: 480 * 1000, F: 30},
						{T: 510 * 1000, F: 30},
						{T: 540 * 1000, F: 30},
						{T: 570 * 1000, F: 30},
						{T: 600 * 1000, F: 30},
					},
				},
			},
		},
		{
			`last_over_time(({app="foo"} |~".+bar" | unwrap foo)[5m])`, time.Unix(5*60, 0), time.Unix(5*120, 0), 30 * time.Second, 0, logproto.BACKWARD, 10,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`)}, // 10 , 20 , 30 .. 300 = 30 total
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(5*120, 0), Selector: `last_over_time({app="foo"}|~".+bar"| unwrap foo[5m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.FromStrings("app", "foo"),
					Floats: []promql.FPoint{
						{T: 300 * 1000, F: 1},
						{T: 330 * 1000, F: 1},
						{T: 360 * 1000, F: 1},
						{T: 390 * 1000, F: 1},
						{T: 420 * 1000, F: 1},
						{T: 450 * 1000, F: 1},
						{T: 480 * 1000, F: 1},
						{T: 510 * 1000, F: 1},
						{T: 540 * 1000, F: 1},
						{T: 570 * 1000, F: 1},
						{T: 600 * 1000, F: 1},
					},
				},
			},
		},
		{
			`avg(count_over_time({app=~"foo|bar"} |~".+bar" [1m]))`, time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`), newSeries(testSize, factor(10, identity), `{app="bar"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `count_over_time({app=~"foo|bar"}|~".+bar"[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.EmptyLabels(),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 6}, {T: 90 * 1000, F: 6}, {T: 120 * 1000, F: 6}, {T: 150 * 1000, F: 6}, {T: 180 * 1000, F: 6}},
				},
			},
		},
		{
			`min(rate({app=~"foo|bar"} |~".+bar" [1m]))`, time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`), newSeries(testSize, factor(10, identity), `{app="bar"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `rate({app=~"foo|bar"}|~".+bar"[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.EmptyLabels(),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 0.1}, {T: 90 * 1000, F: 0.1}, {T: 120 * 1000, F: 0.1}, {T: 150 * 1000, F: 0.1}, {T: 180 * 1000, F: 0.1}},
				},
			},
		},
		{
			`max by (app) (rate({app=~"foo|bar"} |~".+bar" [1m]))`, time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`), newSeries(testSize, factor(5, identity), `{app="bar"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `rate({app=~"foo|bar"}|~".+bar"[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.FromStrings("app", "bar"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 0.2}, {T: 90 * 1000, F: 0.2}, {T: 120 * 1000, F: 0.2}, {T: 150 * 1000, F: 0.2}, {T: 180 * 1000, F: 0.2}},
				},
				promql.Series{
					Metric: labels.FromStrings("app", "foo"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 0.1}, {T: 90 * 1000, F: 0.1}, {T: 120 * 1000, F: 0.1}, {T: 150 * 1000, F: 0.1}, {T: 180 * 1000, F: 0.1}},
				},
			},
		},
		{
			`max(rate({app=~"foo|bar"} |~".+bar" [1m]))`, time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`), newSeries(testSize, factor(5, identity), `{app="bar"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `rate({app=~"foo|bar"}|~".+bar"[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.EmptyLabels(),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 0.2}, {T: 90 * 1000, F: 0.2}, {T: 120 * 1000, F: 0.2}, {T: 150 * 1000, F: 0.2}, {T: 180 * 1000, F: 0.2}},
				},
			},
		},
		{
			`sum(rate({app=~"foo|bar"} |~".+bar" [1m]))`, time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{newSeries(testSize, factor(5, identity), `{app="foo"}`), newSeries(testSize, factor(5, identity), `{app="bar"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `sum(rate({app=~"foo|bar"} |~".+bar" [1m]))`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.EmptyLabels(),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 0.4}, {T: 90 * 1000, F: 0.4}, {T: 120 * 1000, F: 0.4}, {T: 150 * 1000, F: 0.4}, {T: 180 * 1000, F: 0.4}},
				},
			},
		},
		{
			`sum(count_over_time({app=~"foo|bar"} |~".+bar" [1m])) by (app)`, time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`), newSeries(testSize, factor(5, identity), `{app="bar"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `sum by (app) (count_over_time({app=~"foo|bar"} |~".+bar" [1m]))`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.FromStrings("app", "bar"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 12}, {T: 90 * 1000, F: 12}, {T: 120 * 1000, F: 12}, {T: 150 * 1000, F: 12}, {T: 180 * 1000, F: 12}},
				},
				promql.Series{
					Metric: labels.FromStrings("app", "foo"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 6}, {T: 90 * 1000, F: 6}, {T: 120 * 1000, F: 6}, {T: 150 * 1000, F: 6}, {T: 180 * 1000, F: 6}},
				},
			},
		},
		{
			`sum(count_over_time({app=~"foo|bar"} |~".+bar" [1m])) by (namespace,cluster, app)`, time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(10, identity), `{app="foo", cluster="b", namespace="a"}`),
					newSeries(testSize, factor(5, identity), `{app="bar", cluster="a", namespace="b"}`),
					newSeries(testSize, factor(5, identity), `{app="foo", cluster="a" ,namespace="a"}`),
					newSeries(testSize, factor(10, identity), `{app="bar", cluster="b" ,namespace="b"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `sum by (namespace,cluster, app)(count_over_time({app=~"foo|bar"} |~".+bar" [1m]))`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.FromStrings("app", "bar", "cluster", "a", "namespace", "b"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 12}, {T: 90 * 1000, F: 12}, {T: 120 * 1000, F: 12}, {T: 150 * 1000, F: 12}, {T: 180 * 1000, F: 12}},
				},
				promql.Series{
					Metric: labels.FromStrings("app", "bar", "cluster", "b", "namespace", "b"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 6}, {T: 90 * 1000, F: 6}, {T: 120 * 1000, F: 6}, {T: 150 * 1000, F: 6}, {T: 180 * 1000, F: 6}},
				},
				promql.Series{
					Metric: labels.FromStrings("app", "foo", "cluster", "a", "namespace", "a"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 12}, {T: 90 * 1000, F: 12}, {T: 120 * 1000, F: 12}, {T: 150 * 1000, F: 12}, {T: 180 * 1000, F: 12}},
				},
				promql.Series{
					Metric: labels.FromStrings("app", "foo", "cluster", "b", "namespace", "a"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 6}, {T: 90 * 1000, F: 6}, {T: 120 * 1000, F: 6}, {T: 150 * 1000, F: 6}, {T: 180 * 1000, F: 6}},
				},
			},
		},
		{
			`sum(count_over_time({app=~"foo|bar"} |~".+bar" [1m])) by (cluster, namespace, app)`, time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(10, identity), `{app="foo", cluster="b", namespace="a"}`),
					newSeries(testSize, factor(5, identity), `{app="bar", cluster="a", namespace="b"}`),
					newSeries(testSize, factor(5, identity), `{app="foo", cluster="a" ,namespace="a"}`),
					newSeries(testSize, factor(10, identity), `{app="bar", cluster="b" ,namespace="b"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `sum by (cluster, namespace, app) (count_over_time({app=~"foo|bar"} |~".+bar" [1m]))`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.FromStrings("app", "bar", "cluster", "a", "namespace", "b"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 12}, {T: 90 * 1000, F: 12}, {T: 120 * 1000, F: 12}, {T: 150 * 1000, F: 12}, {T: 180 * 1000, F: 12}},
				},
				promql.Series{
					Metric: labels.FromStrings("app", "bar", "cluster", "b", "namespace", "b"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 6}, {T: 90 * 1000, F: 6}, {T: 120 * 1000, F: 6}, {T: 150 * 1000, F: 6}, {T: 180 * 1000, F: 6}},
				},
				promql.Series{
					Metric: labels.FromStrings("app", "foo", "cluster", "a", "namespace", "a"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 12}, {T: 90 * 1000, F: 12}, {T: 120 * 1000, F: 12}, {T: 150 * 1000, F: 12}, {T: 180 * 1000, F: 12}},
				},
				promql.Series{
					Metric: labels.FromStrings("app", "foo", "cluster", "b", "namespace", "a"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 6}, {T: 90 * 1000, F: 6}, {T: 120 * 1000, F: 6}, {T: 150 * 1000, F: 6}, {T: 180 * 1000, F: 6}},
				},
			},
		},
		{
			`sum(count_over_time({app=~"foo|bar"} |~".+bar" [1m])) by (namespace, app)`, time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(10, identity), `{app="foo", cluster="b", namespace="a"}`),
					newSeries(testSize, factor(5, identity), `{app="bar", cluster="a", namespace="b"}`),
					newSeries(testSize, factor(5, identity), `{app="foo", cluster="a" ,namespace="a"}`),
					newSeries(testSize, factor(10, identity), `{app="bar", cluster="b" ,namespace="b"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `sum by (namespace, app)(count_over_time({app=~"foo|bar"} |~".+bar" [1m]))`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.FromStrings("app", "bar", "namespace", "b"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 18}, {T: 90 * 1000, F: 18}, {T: 120 * 1000, F: 18}, {T: 150 * 1000, F: 18}, {T: 180 * 1000, F: 18}},
				},
				promql.Series{
					Metric: labels.FromStrings("app", "foo", "namespace", "a"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 18}, {T: 90 * 1000, F: 18}, {T: 120 * 1000, F: 18}, {T: 150 * 1000, F: 18}, {T: 180 * 1000, F: 18}},
				},
			},
		},
		{
			`count(count_over_time({app=~"foo|bar"} |~".+bar" [1m])) without (app)`, time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`), newSeries(testSize, factor(10, identity), `{app="bar"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `count_over_time({app=~"foo|bar"}|~".+bar"[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.EmptyLabels(),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 2}, {T: 90 * 1000, F: 2}, {T: 120 * 1000, F: 2}, {T: 150 * 1000, F: 2}, {T: 180 * 1000, F: 2}},
				},
			},
		},
		{
			`stdvar without (app) (count_over_time(({app=~"foo|bar"} |~".+bar")[1m])) `, time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`), newSeries(testSize, factor(5, identity), `{app="bar"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `count_over_time({app=~"foo|bar"}|~".+bar"[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.EmptyLabels(),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 9}, {T: 90 * 1000, F: 9}, {T: 120 * 1000, F: 9}, {T: 150 * 1000, F: 9}, {T: 180 * 1000, F: 9}},
				},
			},
		},
		{
			`stddev(count_over_time(({app=~"foo|bar"} |~".+bar")[1m])) `, time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`), newSeries(testSize, factor(2, identity), `{app="bar"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `count_over_time({app=~"foo|bar"}|~".+bar"[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.EmptyLabels(),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 12}, {T: 90 * 1000, F: 12}, {T: 120 * 1000, F: 12}, {T: 150 * 1000, F: 12}, {T: 180 * 1000, F: 12}},
				},
			},
		},
		{
			`rate(({app=~"foo|bar"} |~".+bar")[1m])`, time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`), newSeries(testSize, factor(5, identity), `{app="bar"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `rate({app=~"foo|bar"}|~".+bar"[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.FromStrings("app", "bar"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 0.2}, {T: 90 * 1000, F: 0.2}, {T: 120 * 1000, F: 0.2}, {T: 150 * 1000, F: 0.2}, {T: 180 * 1000, F: 0.2}},
				},
				promql.Series{
					Metric: labels.FromStrings("app", "foo"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 0.1}, {T: 90 * 1000, F: 0.1}, {T: 120 * 1000, F: 0.1}, {T: 150 * 1000, F: 0.1}, {T: 180 * 1000, F: 0.1}},
				},
			},
		},
		{
			`absent_over_time(({app="foo"} |~".+bar")[1m])`, time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{newSeries(1, constant(50), `{app="foo"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `absent_over_time({app="foo"}|~".+bar"[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.FromStrings("app", "foo"),
					Floats: []promql.FPoint{
						{T: 120000, F: 1}, {T: 150000, F: 1}, {T: 180000, F: 1},
					},
				},
			},
		},
		{
			`rate(({app=~"foo|bar"} |~".+bar" | unwrap bar)[1m])`, time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(10, constantValue(2)), `{app="foo"}`),
					newSeries(testSize, factor(5, constantValue(2)), `{app="bar"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `rate({app=~"foo|bar"}|~".+bar"|unwrap bar[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.FromStrings("app", "bar"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 0.4}, {T: 90 * 1000, F: 0.4}, {T: 120 * 1000, F: 0.4}, {T: 150 * 1000, F: 0.4}, {T: 180 * 1000, F: 0.4}},
				},
				promql.Series{
					Metric: labels.FromStrings("app", "foo"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 0.2}, {T: 90 * 1000, F: 0.2}, {T: 120 * 1000, F: 0.2}, {T: 150 * 1000, F: 0.2}, {T: 180 * 1000, F: 0.2}},
				},
			},
		},
		{
			`topk(2,rate(({app=~"foo|bar"} |~".+bar")[1m]))`, time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`), newSeries(testSize, factor(5, identity), `{app="bar"}`), newSeries(testSize, factor(15, identity), `{app="boo"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `rate({app=~"foo|bar"}|~".+bar"[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.FromStrings("app", "bar"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 0.2}, {T: 90 * 1000, F: 0.2}, {T: 120 * 1000, F: 0.2}, {T: 150 * 1000, F: 0.2}, {T: 180 * 1000, F: 0.2}},
				},
				promql.Series{
					Metric: labels.FromStrings("app", "foo"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 0.1}, {T: 90 * 1000, F: 0.1}, {T: 120 * 1000, F: 0.1}, {T: 150 * 1000, F: 0.1}, {T: 180 * 1000, F: 0.1}},
				},
			},
		},
		{
			`topk(1,rate(({app=~"foo|bar"} |~".+bar")[1m]))`, time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{newSeries(testSize, factor(10, identity), `{app="foo"}`), newSeries(testSize, factor(5, identity), `{app="bar"}`)},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `rate({app=~"foo|bar"}|~".+bar"[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.FromStrings("app", "bar"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 0.2}, {T: 90 * 1000, F: 0.2}, {T: 120 * 1000, F: 0.2}, {T: 150 * 1000, F: 0.2}, {T: 180 * 1000, F: 0.2}},
				},
			},
		},
		{
			`topk(1,rate(({app=~"foo|bar"} |~".+bar")[1m])) by (app)`, time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(10, identity), `{app="foo"}`),
					newSeries(testSize, factor(5, identity), `{app="fuzz"}`), newSeries(testSize, identity, `{app="buzz"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `rate({app=~"foo|bar"}|~".+bar"[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.FromStrings("app", "buzz"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 1}, {T: 90 * 1000, F: 1}, {T: 120 * 1000, F: 1}, {T: 150 * 1000, F: 1}, {T: 180 * 1000, F: 1}},
				},
				promql.Series{
					Metric: labels.FromStrings("app", "foo"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 0.1}, {T: 90 * 1000, F: 0.1}, {T: 120 * 1000, F: 0.1}, {T: 150 * 1000, F: 0.1}, {T: 180 * 1000, F: 0.1}},
				},
				promql.Series{
					Metric: labels.FromStrings("app", "fuzz"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 0.2}, {T: 90 * 1000, F: 0.2}, {T: 120 * 1000, F: 0.2}, {T: 150 * 1000, F: 0.2}, {T: 180 * 1000, F: 0.2}},
				},
			},
		},
		{
			`bottomk(2,rate(({app=~"foo|bar"} |~".+bar")[1m]))`, time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(10, identity), `{app="foo"}`), newSeries(testSize, factor(20, identity), `{app="bar"}`),
					newSeries(testSize, factor(5, identity), `{app="fuzz"}`), newSeries(testSize, identity, `{app="buzz"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `rate({app=~"foo|bar"}|~".+bar"[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.FromStrings("app", "bar"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 0.05}, {T: 90 * 1000, F: 0.05}, {T: 120 * 1000, F: 0.05}, {T: 150 * 1000, F: 0.05}, {T: 180 * 1000, F: 0.05}},
				},
				promql.Series{
					Metric: labels.FromStrings("app", "foo"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 0.1}, {T: 90 * 1000, F: 0.1}, {T: 120 * 1000, F: 0.1}, {T: 150 * 1000, F: 0.1}, {T: 180 * 1000, F: 0.1}},
				},
			},
		},
		{
			`bottomk(3,rate(({app=~"foo|bar|fuzz|buzz"} |~".+bar")[1m])) without (app)`, time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(10, identity), `{app="foo"}`),
					newSeries(testSize, factor(20, identity), `{app="bar"}`),
					newSeries(testSize, factor(5, identity), `{app="fuzz"}`),
					newSeries(testSize, identity, `{app="buzz"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `rate({app=~"foo|bar|fuzz|buzz"}|~".+bar"[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.FromStrings("app", "bar"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 0.05}, {T: 90 * 1000, F: 0.05}, {T: 120 * 1000, F: 0.05}, {T: 150 * 1000, F: 0.05}, {T: 180 * 1000, F: 0.05}},
				},
				promql.Series{
					Metric: labels.FromStrings("app", "foo"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 0.1}, {T: 90 * 1000, F: 0.1}, {T: 120 * 1000, F: 0.1}, {T: 150 * 1000, F: 0.1}, {T: 180 * 1000, F: 0.1}},
				},
				promql.Series{
					Metric: labels.FromStrings("app", "fuzz"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 0.2}, {T: 90 * 1000, F: 0.2}, {T: 120 * 1000, F: 0.2}, {T: 150 * 1000, F: 0.2}, {T: 180 * 1000, F: 0.2}},
				},
			},
		},
		// binops
		{
			`rate({app="foo"}[1m]) or rate({app="bar"}[1m])`,
			time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(5, identity), `{app="foo"}`),
				},
				{
					newSeries(testSize, factor(5, identity), `{app="bar"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `rate({app="foo"}[1m])`}},
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `rate({app="bar"}[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.FromStrings("app", "bar"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 0.2}, {T: 90 * 1000, F: 0.2}, {T: 120 * 1000, F: 0.2}, {T: 150 * 1000, F: 0.2}, {T: 180 * 1000, F: 0.2}},
				},
				promql.Series{
					Metric: labels.FromStrings("app", "foo"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 0.2}, {T: 90 * 1000, F: 0.2}, {T: 120 * 1000, F: 0.2}, {T: 150 * 1000, F: 0.2}, {T: 180 * 1000, F: 0.2}},
				},
			},
		},
		{
			`rate({app="foo"}[1m]) or vector(0)`,
			time.Unix(60, 0), time.Unix(180, 0), 20 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{logproto.Series{
					Labels: `{app="foo"}`,
					Samples: []logproto.Sample{
						{Timestamp: time.Unix(55, 0).UnixNano(), Hash: 1, Value: 1.},
						{Timestamp: time.Unix(60, 0).UnixNano(), Hash: 2, Value: 1.},
						{Timestamp: time.Unix(65, 0).UnixNano(), Hash: 3, Value: 1.},
						{Timestamp: time.Unix(70, 0).UnixNano(), Hash: 4, Value: 1.},
						{Timestamp: time.Unix(170, 0).UnixNano(), Hash: 5, Value: 1.},
						{Timestamp: time.Unix(175, 0).UnixNano(), Hash: 6, Value: 1.},
					},
				}},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `rate({app="foo"}[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					// vector result
					Metric: labels.EmptyLabels(),
					Floats: []promql.FPoint{{T: 60000, F: 0}, {T: 80000, F: 0}, {T: 100000, F: 0}, {T: 120000, F: 0}, {T: 140000, F: 0}, {T: 160000, F: 0}, {T: 180000, F: 0}},
				},
				promql.Series{
					Metric: labels.FromStrings("app", "foo"),
					Floats: []promql.FPoint{{T: 60000, F: 0.03333333333333333}, {T: 80000, F: 0.06666666666666667}, {T: 100000, F: 0.06666666666666667}, {T: 120000, F: 0.03333333333333333}, {T: 180000, F: 0.03333333333333333}},
				},
			},
		},
		{
			`
			rate({app=~"foo|bar"}[1m]) and
			rate({app="bar"}[1m])
			`,
			time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(5, identity), `{app="foo"}`),
					newSeries(testSize, factor(5, identity), `{app="bar"}`),
				},
				{
					newSeries(testSize, factor(5, identity), `{app="bar"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `rate({app=~"foo|bar"}[1m])`}},
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `rate({app="bar"}[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.FromStrings("app", "bar"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 0.2}, {T: 90 * 1000, F: 0.2}, {T: 120 * 1000, F: 0.2}, {T: 150 * 1000, F: 0.2}, {T: 180 * 1000, F: 0.2}},
				},
			},
		},
		{
			`
			rate({app=~"foo|bar"}[1m]) unless
			rate({app="bar"}[1m])
			`,
			time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(5, identity), `{app="foo"}`),
					newSeries(testSize, factor(5, identity), `{app="bar"}`),
				},
				{
					newSeries(testSize, factor(5, identity), `{app="bar"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `rate({app=~"foo|bar"}[1m])`}},
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `rate({app="bar"}[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.FromStrings("app", "foo"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 0.2}, {T: 90 * 1000, F: 0.2}, {T: 120 * 1000, F: 0.2}, {T: 150 * 1000, F: 0.2}, {T: 180 * 1000, F: 0.2}},
				},
			},
		},
		{
			`
			rate({app=~"foo|bar"}[1m]) +
			rate({app="bar"}[1m])
			`,
			time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(5, identity), `{app="foo"}`),
					newSeries(testSize, factor(5, identity), `{app="bar"}`),
				},
				{
					newSeries(testSize, factor(5, identity), `{app="bar"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `rate({app=~"foo|bar"}[1m])`}},
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `rate({app="bar"}[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.FromStrings("app", "bar"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 0.4}, {T: 90 * 1000, F: 0.4}, {T: 120 * 1000, F: 0.4}, {T: 150 * 1000, F: 0.4}, {T: 180 * 1000, F: 0.4}},
				},
			},
		},
		{
			`
			rate({app=~"foo|bar"}[1m]) -
			rate({app="bar"}[1m])
			`,
			time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(5, identity), `{app="foo"}`),
					newSeries(testSize, factor(5, identity), `{app="bar"}`),
				},
				{
					newSeries(testSize, factor(5, identity), `{app="bar"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `rate({app=~"foo|bar"}[1m])`}},
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `rate({app="bar"}[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.FromStrings("app", "bar"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 0}, {T: 90 * 1000, F: 0}, {T: 120 * 1000, F: 0}, {T: 150 * 1000, F: 0}, {T: 180 * 1000, F: 0}},
				},
			},
		},
		{
			`
			count_over_time({app=~"foo|bar"}[1m]) *
			count_over_time({app="bar"}[1m])
			`,
			time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(5, identity), `{app="foo"}`),
					newSeries(testSize, factor(5, identity), `{app="bar"}`),
				},
				{
					newSeries(testSize, factor(5, identity), `{app="bar"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `count_over_time({app=~"foo|bar"}[1m])`}},
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `count_over_time({app="bar"}[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.FromStrings("app", "bar"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 144}, {T: 90 * 1000, F: 144}, {T: 120 * 1000, F: 144}, {T: 150 * 1000, F: 144}, {T: 180 * 1000, F: 144}},
				},
			},
		},
		{
			`
			count_over_time({app=~"foo|bar"}[1m]) *
			count_over_time({app="bar"}[1m])
			`,
			time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(5, identity), `{app="foo"}`),
					newSeries(testSize, factor(5, identity), `{app="bar"}`),
				},
				{
					newSeries(testSize, factor(5, identity), `{app="bar"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `count_over_time({app=~"foo|bar"}[1m])`}},
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `count_over_time({app="bar"}[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.FromStrings("app", "bar"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 144}, {T: 90 * 1000, F: 144}, {T: 120 * 1000, F: 144}, {T: 150 * 1000, F: 144}, {T: 180 * 1000, F: 144}},
				},
			},
		},
		{
			`
			count_over_time({app=~"foo|bar"}[1m]) /
			count_over_time({app="bar"}[1m])
			`,
			time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(5, identity), `{app="foo"}`),
					newSeries(testSize, factor(5, identity), `{app="bar"}`),
				},
				{
					newSeries(testSize, factor(5, identity), `{app="bar"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `count_over_time({app=~"foo|bar"}[1m])`}},
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `count_over_time({app="bar"}[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.FromStrings("app", "bar"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 1}, {T: 90 * 1000, F: 1}, {T: 120 * 1000, F: 1}, {T: 150 * 1000, F: 1}, {T: 180 * 1000, F: 1}},
				},
			},
		},
		{
			`
			count_over_time({app=~"foo|bar"}[1m]) %
			count_over_time({app="bar"}[1m])
			`,
			time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(5, identity), `{app="foo"}`),
					newSeries(testSize, factor(5, identity), `{app="bar"}`),
				},
				{
					newSeries(testSize, factor(5, identity), `{app="bar"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `count_over_time({app=~"foo|bar"}[1m])`}},
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `count_over_time({app="bar"}[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.FromStrings("app", "bar"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 0}, {T: 90 * 1000, F: 0}, {T: 120 * 1000, F: 0}, {T: 150 * 1000, F: 0}, {T: 180 * 1000, F: 0}},
				},
			},
		},
		// tests precedence: should be x + (x/x)
		{
			`
			sum by (app) (rate({app=~"foo|bar"} |~".+bar" [1m])) +
			sum by (app) (rate({app=~"foo|bar"} |~".+bar" [1m])) /
			sum by (app) (rate({app=~"foo|bar"} |~".+bar" [1m]))
			`,
			time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(5, identity), `{app="foo"}`),
					newSeries(testSize, factor(5, identity), `{app="bar"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `sum by (app) (rate({app=~"foo|bar"} |~".+bar" [1m]))`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.FromStrings("app", "bar"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 1.2}, {T: 90 * 1000, F: 1.2}, {T: 120 * 1000, F: 1.2}, {T: 150 * 1000, F: 1.2}, {T: 180 * 1000, F: 1.2}},
				},
				promql.Series{
					Metric: labels.FromStrings("app", "foo"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 1.2}, {T: 90 * 1000, F: 1.2}, {T: 120 * 1000, F: 1.2}, {T: 150 * 1000, F: 1.2}, {T: 180 * 1000, F: 1.2}},
				},
			},
		},
		{
			`avg by (app) (
				sum by (app) (rate({app=~"foo|bar"} |~".+bar" [1m])) +
				sum by (app) (rate({app=~"foo|bar"} |~".+bar" [1m])) /
				sum by (app) (rate({app=~"foo|bar"} |~".+bar" [1m]))
				) * 2
			`,
			time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(5, identity), `{app="foo"}`),
					newSeries(testSize, factor(5, identity), `{app="bar"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `sum by (app) (rate({app=~"foo|bar"} |~".+bar" [1m]))`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.FromStrings("app", "bar"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 2.4}, {T: 90 * 1000, F: 2.4}, {T: 120 * 1000, F: 2.4}, {T: 150 * 1000, F: 2.4}, {T: 180 * 1000, F: 2.4}},
				},
				promql.Series{
					Metric: labels.FromStrings("app", "foo"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 2.4}, {T: 90 * 1000, F: 2.4}, {T: 120 * 1000, F: 2.4}, {T: 150 * 1000, F: 2.4}, {T: 180 * 1000, F: 2.4}},
				},
			},
		},
		{
			`label_replace(
				avg by (app) (
					sum by (app) (rate({app=~"foo|bar"} |~".+bar" [1m])) +
					sum by (app) (rate({app=~"foo|bar"} |~".+bar" [1m])) /
					sum by (app) (rate({app=~"foo|bar"} |~".+bar" [1m]))
					) * 2,
				"new",
				"$1",
				"app",
				"f(.*)"
			)
			`,
			time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(5, identity), `{app="foo"}`),
					newSeries(testSize, factor(5, identity), `{app="bar"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `sum by (app) (rate({app=~"foo|bar"} |~".+bar" [1m]))`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.FromStrings("app", "bar"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 2.4}, {T: 90 * 1000, F: 2.4}, {T: 120 * 1000, F: 2.4}, {T: 150 * 1000, F: 2.4}, {T: 180 * 1000, F: 2.4}},
				},
				promql.Series{
					Metric: labels.FromStrings("app", "foo", "new", "oo"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 2.4}, {T: 90 * 1000, F: 2.4}, {T: 120 * 1000, F: 2.4}, {T: 150 * 1000, F: 2.4}, {T: 180 * 1000, F: 2.4}},
				},
			},
		},
		{
			` sum (
					sum by (app) (rate({app=~"foo|bar"} |~".+bar" [1m])) +
					sum by (app) (rate({app=~"foo|bar"} |~".+bar" [1m])) /
					sum by (app) (rate({app=~"foo|bar"} |~".+bar" [1m]))
			) + 1
		`,
			time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(5, identity), `{app="foo"}`),
					newSeries(testSize, factor(5, identity), `{app="bar"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `sum by (app) (rate({app=~"foo|bar"} |~".+bar" [1m]))`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.EmptyLabels(),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 3.4}, {T: 90 * 1000, F: 3.4}, {T: 120 * 1000, F: 3.4}, {T: 150 * 1000, F: 3.4}, {T: 180 * 1000, F: 3.4}},
				},
			},
		},
		{
			`1+1--1`,
			time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			nil,
			nil,
			promql.Matrix{
				promql.Series{
					Floats: []promql.FPoint{{T: 60000, F: 3}, {T: 90000, F: 3}, {T: 120000, F: 3}, {T: 150000, F: 3}, {T: 180000, F: 3}},
				},
			},
		},
		{
			`rate({app="bar"}[1m]) - 1`,
			time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(5, identity), `{app="bar"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `rate({app="bar"}[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.FromStrings("app", "bar"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: -0.8}, {T: 90 * 1000, F: -0.8}, {T: 120 * 1000, F: -0.8}, {T: 150 * 1000, F: -0.8}, {T: 180 * 1000, F: -0.8}},
				},
			},
		},
		{
			`1 - rate({app="bar"}[1m])`,
			time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(5, identity), `{app="bar"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `rate({app="bar"}[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.FromStrings("app", "bar"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 0.8}, {T: 90 * 1000, F: 0.8}, {T: 120 * 1000, F: 0.8}, {T: 150 * 1000, F: 0.8}, {T: 180 * 1000, F: 0.8}},
				},
			},
		},
		{
			`rate({app="bar"}[1m]) - 1 / 2`,
			time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(5, identity), `{app="bar"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `rate({app="bar"}[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.FromStrings("app", "bar"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: -0.3}, {T: 90 * 1000, F: -0.3}, {T: 120 * 1000, F: -0.3}, {T: 150 * 1000, F: -0.3}, {T: 180 * 1000, F: -0.3}},
				},
			},
		},
		{
			`count_over_time({app="bar"}[1m]) ^ count_over_time({app="bar"}[1m])`,
			time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			[][]logproto.Series{
				{
					newSeries(testSize, factor(5, identity), `{app="bar"}`),
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(0, 0), End: time.Unix(180, 0), Selector: `count_over_time({app="bar"}[1m])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.FromStrings("app", "bar"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: math.Pow(12, 12)}, {T: 90 * 1000, F: math.Pow(12, 12)}, {T: 120 * 1000, F: math.Pow(12, 12)}, {T: 150 * 1000, F: math.Pow(12, 12)}, {T: 180 * 1000, F: math.Pow(12, 12)}},
				},
			},
		},
		{
			`2`,
			time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			nil,
			nil,
			promql.Matrix{
				promql.Series{
					Floats: []promql.FPoint{{T: 60 * 1000, F: 2}, {T: 90 * 1000, F: 2}, {T: 120 * 1000, F: 2}, {T: 150 * 1000, F: 2}, {T: 180 * 1000, F: 2}},
				},
			},
		},
		// vector query range
		{
			`vector(2)`,
			time.Unix(60, 0), time.Unix(180, 0), 30 * time.Second, 0, logproto.FORWARD, 100,
			nil,
			nil,
			promql.Matrix{
				promql.Series{
					Floats: []promql.FPoint{{T: 60 * 1000, F: 2}, {T: 90 * 1000, F: 2}, {T: 120 * 1000, F: 2}, {T: 150 * 1000, F: 2}, {T: 180 * 1000, F: 2}},
				},
			},
		},
		{
			`bytes_rate({app="foo"}[30s])`, time.Unix(60, 0), time.Unix(120, 0), 15 * time.Second, 0, logproto.FORWARD, 10,
			[][]logproto.Series{
				{logproto.Series{
					Labels: `{app="foo"}`,
					Samples: []logproto.Sample{
						{Timestamp: time.Unix(45, 0).UnixNano(), Hash: 1, Value: 10.}, // 10 bytes / 30s for the first point.
						{Timestamp: time.Unix(60, 0).UnixNano(), Hash: 2, Value: 0.},
						{Timestamp: time.Unix(75, 0).UnixNano(), Hash: 3, Value: 0.},
						{Timestamp: time.Unix(90, 0).UnixNano(), Hash: 4, Value: 0.},
						{Timestamp: time.Unix(105, 0).UnixNano(), Hash: 5, Value: 0.},
					},
				}},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(30, 0), End: time.Unix(120, 0), Selector: `bytes_rate({app="foo"}[30s])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.FromStrings("app", "foo"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 10. / 30.}, {T: 75 * 1000, F: 0}, {T: 90 * 1000, F: 0}, {T: 105 * 1000, F: 0}, {T: 120 * 1000, F: 0}},
				},
			},
		},
		{
			`bytes_over_time({app="foo"}[30s])`, time.Unix(60, 0), time.Unix(120, 0), 15 * time.Second, 0, logproto.FORWARD, 10,
			[][]logproto.Series{
				{logproto.Series{
					Labels: `{app="foo"}`,
					Samples: []logproto.Sample{
						{Timestamp: time.Unix(45, 0).UnixNano(), Hash: 1, Value: 5.}, // 5 bytes
						{Timestamp: time.Unix(60, 0).UnixNano(), Hash: 2, Value: 0.},
						{Timestamp: time.Unix(75, 0).UnixNano(), Hash: 3, Value: 0.},
						{Timestamp: time.Unix(90, 0).UnixNano(), Hash: 4, Value: 0.},
						{Timestamp: time.Unix(105, 0).UnixNano(), Hash: 5, Value: 4.}, // 4 bytes
					},
				}},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(30, 0), End: time.Unix(120, 0), Selector: `bytes_over_time({app="foo"}[30s])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.FromStrings("app", "foo"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 5.}, {T: 75 * 1000, F: 0}, {T: 90 * 1000, F: 0}, {T: 105 * 1000, F: 4.}, {T: 120 * 1000, F: 4.}},
				},
			},
		},
		{
			`bytes_over_time({app="foo"}[30s]) > bool 1`, time.Unix(60, 0), time.Unix(120, 0), 15 * time.Second, 0, logproto.FORWARD, 10,
			[][]logproto.Series{
				{logproto.Series{
					Labels: `{app="foo"}`,
					Samples: []logproto.Sample{
						{Timestamp: time.Unix(45, 0).UnixNano(), Hash: 1, Value: 5.}, // 5 bytes
						{Timestamp: time.Unix(60, 0).UnixNano(), Hash: 2, Value: 0.},
						{Timestamp: time.Unix(75, 0).UnixNano(), Hash: 3, Value: 0.},
						{Timestamp: time.Unix(90, 0).UnixNano(), Hash: 4, Value: 0.},
						{Timestamp: time.Unix(105, 0).UnixNano(), Hash: 5, Value: 4.}, // 4 bytes
					},
				}},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(30, 0), End: time.Unix(120, 0), Selector: `bytes_over_time({app="foo"}[30s])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.FromStrings("app", "foo"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 1.}, {T: 75 * 1000, F: 0}, {T: 90 * 1000, F: 0}, {T: 105 * 1000, F: 1.}, {T: 120 * 1000, F: 1.}},
				},
			},
		},
		{
			`bytes_over_time({app="foo"}[30s]) > 1`, time.Unix(60, 0), time.Unix(120, 0), 15 * time.Second, 0, logproto.FORWARD, 10,
			[][]logproto.Series{
				{logproto.Series{
					Labels: `{app="foo"}`,
					Samples: []logproto.Sample{
						{Timestamp: time.Unix(45, 0).UnixNano(), Hash: 1, Value: 5.}, // 5 bytes
						{Timestamp: time.Unix(60, 0).UnixNano(), Hash: 2, Value: 0.},
						{Timestamp: time.Unix(75, 0).UnixNano(), Hash: 3, Value: 0.},
						{Timestamp: time.Unix(90, 0).UnixNano(), Hash: 4, Value: 0.},
						{Timestamp: time.Unix(105, 0).UnixNano(), Hash: 5, Value: 4.}, // 4 bytes
					},
				}},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(30, 0), End: time.Unix(120, 0), Selector: `bytes_over_time({app="foo"}[30s])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.FromStrings("app", "foo"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 5.}, {T: 105 * 1000, F: 4.}, {T: 120 * 1000, F: 4.}},
				},
			},
		},
		{
			// should return same results as `bytes_over_time({app="foo"}[30s]) > 1`.
			// https://grafana.com/docs/loki/latest/query/#comparison-operators
			// Between a vector and a scalar, these operators are
			// applied to the value of every data sample in the vector
			`1 < bytes_over_time({app="foo"}[30s])`, time.Unix(60, 0), time.Unix(120, 0), 15 * time.Second, 0, logproto.FORWARD, 10,
			[][]logproto.Series{
				{logproto.Series{
					Labels: `{app="foo"}`,
					Samples: []logproto.Sample{
						{Timestamp: time.Unix(45, 0).UnixNano(), Hash: 1, Value: 5.}, // 5 bytes
						{Timestamp: time.Unix(60, 0).UnixNano(), Hash: 2, Value: 0.},
						{Timestamp: time.Unix(75, 0).UnixNano(), Hash: 3, Value: 0.},
						{Timestamp: time.Unix(90, 0).UnixNano(), Hash: 4, Value: 0.},
						{Timestamp: time.Unix(105, 0).UnixNano(), Hash: 5, Value: 4.}, // 4 bytes
					},
				}},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(30, 0), End: time.Unix(120, 0), Selector: `bytes_over_time({app="foo"}[30s])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.FromStrings("app", "foo"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 5.}, {T: 105 * 1000, F: 4.}, {T: 120 * 1000, F: 4.}},
				},
			},
		},
		{
			`bytes_over_time({app="foo"}[30s]) > bool 1`, time.Unix(60, 0), time.Unix(120, 0), 15 * time.Second, 0, logproto.FORWARD, 10,
			[][]logproto.Series{
				{logproto.Series{
					Labels: `{app="foo"}`,
					Samples: []logproto.Sample{
						{Timestamp: time.Unix(45, 0).UnixNano(), Hash: 1, Value: 5.}, // 5 bytes
						{Timestamp: time.Unix(60, 0).UnixNano(), Hash: 2, Value: 0.},
						{Timestamp: time.Unix(75, 0).UnixNano(), Hash: 3, Value: 0.},
						{Timestamp: time.Unix(90, 0).UnixNano(), Hash: 4, Value: 0.},
						{Timestamp: time.Unix(105, 0).UnixNano(), Hash: 5, Value: 4.}, // 4 bytes
					},
				}},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(30, 0), End: time.Unix(120, 0), Selector: `bytes_over_time({app="foo"}[30s])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.FromStrings("app", "foo"),
					Floats: []promql.FPoint{
						{T: 60000, F: 1},
						{T: 75000, F: 0},
						{T: 90000, F: 0},
						{T: 105000, F: 1},
						{T: 120000, F: 1},
					},
				},
			},
		},
		{
			// should return same results as `bytes_over_time({app="foo"}[30s]) > bool 1`.
			// https://grafana.com/docs/loki/latest/query/#comparison-operators
			// Between a vector and a scalar, these operators are
			// applied to the value of every data sample in the vector
			`1 < bool bytes_over_time({app="foo"}[30s])`, time.Unix(60, 0), time.Unix(120, 0), 15 * time.Second, 0, logproto.FORWARD, 10,
			[][]logproto.Series{
				{logproto.Series{
					Labels: `{app="foo"}`,
					Samples: []logproto.Sample{
						{Timestamp: time.Unix(45, 0).UnixNano(), Hash: 1, Value: 5.}, // 5 bytes
						{Timestamp: time.Unix(60, 0).UnixNano(), Hash: 2, Value: 0.},
						{Timestamp: time.Unix(75, 0).UnixNano(), Hash: 3, Value: 0.},
						{Timestamp: time.Unix(90, 0).UnixNano(), Hash: 4, Value: 0.},
						{Timestamp: time.Unix(105, 0).UnixNano(), Hash: 5, Value: 4.}, // 4 bytes
					},
				}},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(30, 0), End: time.Unix(120, 0), Selector: `bytes_over_time({app="foo"}[30s])`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.FromStrings("app", "foo"),
					Floats: []promql.FPoint{
						{T: 60000, F: 1},
						{T: 75000, F: 0},
						{T: 90000, F: 0},
						{T: 105000, F: 1},
						{T: 120000, F: 1},
					},
				},
			},
		},
		{
			// tests combining two streams + unwrap
			`sum(rate({job="foo"} | logfmt | bar > 0 | unwrap bazz [30s]))`, time.Unix(60, 0), time.Unix(120, 0), 30 * time.Second, 0, logproto.FORWARD, 10,
			[][]logproto.Series{
				{
					{
						Labels: `{job="foo", bar="1"}`,
						Samples: []logproto.Sample{
							{Timestamp: time.Unix(40, 0).UnixNano(), Hash: 1, Value: 0.},
							{Timestamp: time.Unix(45, 0).UnixNano(), Hash: 1, Value: 10.},
							{Timestamp: time.Unix(60, 0).UnixNano(), Hash: 2, Value: 0.},
							{Timestamp: time.Unix(90, 0).UnixNano(), Hash: 2, Value: 0.},
							{Timestamp: time.Unix(120, 0).UnixNano(), Hash: 2, Value: 0.},
						},
					},
					{
						Labels: `{job="foo", bar="2"}`,
						Samples: []logproto.Sample{
							{Timestamp: time.Unix(40, 0).UnixNano(), Hash: 1, Value: 0.},
							{Timestamp: time.Unix(45, 0).UnixNano(), Hash: 1, Value: 10.},
							{Timestamp: time.Unix(60, 0).UnixNano(), Hash: 2, Value: 0.},
							{Timestamp: time.Unix(90, 0).UnixNano(), Hash: 2, Value: 0.},
							{Timestamp: time.Unix(120, 0).UnixNano(), Hash: 2, Value: 0.},
						},
					},
				},
			},
			[]SelectSampleParams{
				{&logproto.SampleQueryRequest{Start: time.Unix(30, 0), End: time.Unix(120, 0), Selector: `sum(rate({job="foo"} | logfmt | bar > 0  | unwrap bazz [30s]))`}},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.EmptyLabels(),
					Floats: []promql.FPoint{
						{T: 60000, F: 20. / 30.},
						{T: 90000, F: 0},
						{T: 120000, F: 0},
					},
				},
			},
		},
	} {
		t.Run(fmt.Sprintf("%s %s", test.qs, test.direction), func(t *testing.T) {
			t.Parallel()

			eng := NewEngine(EngineOpts{}, newQuerierRecorder(t, test.data, test.params), NoLimits, log.NewNopLogger())

			params, err := NewLiteralParams(test.qs, test.start, test.end, test.step, test.interval, test.direction, test.limit, nil, nil)
			require.NoError(t, err)
			q := eng.Query(params)
			res, err := q.Exec(user.InjectOrgID(context.Background(), "fake"))
			if err != nil {
				t.Fatal(err)
			}
			assert.Equal(t, test.expected, res.Data)
		})
	}
}

func TestEngine_Variants_InstantQuery(t *testing.T) {
	t.Parallel()

	// Create a custom fakeLimits to enable multi-variant queries
	customLimits := &fakeLimits{
		maxSeries:               math.MaxInt32,
		timeout:                 time.Hour,
		multiVariantQueryEnable: true,
	}

	for _, test := range []struct {
		qs        string
		ts        time.Time
		direction logproto.Direction
		limit     uint32

		// an array of data per params will be returned by the querier.
		// This is to cover logql that requires multiple queries.
		data   interface{}
		params interface{}

		expected interface{}
	}{
		{
			`variants(bytes_over_time({app="foo"}[1m]), count_over_time({app="foo"}[1m])) of ({app="foo"}[1m])`,
			time.Unix(60, 0),
			logproto.BACKWARD,
			0,
			[][]logproto.Series{
				{newSeries(testSize, identity, `{app="foo"}`)},
			},
			[]SelectSampleParams{
				{
					&logproto.SampleQueryRequest{
						Selector: `variants(bytes_over_time({app="foo"}[1m]), count_over_time({app="foo"}[1m])) of ({app="foo"}[1m])`,
						Plan: &plan.QueryPlan{
							AST: syntax.MustParseExpr(`variants(bytes_over_time({app="foo"}[1m]), count_over_time({app="foo"}[1m])) of ({app="foo"}[1m])`),
						},
						Start: time.Unix(0, 0),
						End:   time.Unix(60, 0),
					},
				},
			},
			promql.Vector{
				promql.Sample{T: 60 * 1000, F: 60, Metric: labels.FromStrings(constants.VariantLabel, "0", "app", "foo")},
				promql.Sample{T: 60 * 1000, F: 60, Metric: labels.FromStrings(constants.VariantLabel, "1", "app", "foo")},
			},
		},
		{
			`variants(sum by (app) (bytes_over_time({app="foo"}[1m])), sum by (app) (count_over_time({app="foo"}[1m]))) of ({app="foo"}[1m])`,
			time.Unix(60, 0),
			logproto.BACKWARD,
			0,
			[][]logproto.Series{
				{
					newSeries(testSize, identity, `{app="foo", foo="bar"}`),
					newSeries(testSize, identity, `{app="foo", foo="baz"}`),
				},
			},
			[]SelectSampleParams{
				{
					&logproto.SampleQueryRequest{
						Selector: `variants(sum by (app) (bytes_over_time({app="foo"}[1m])), sum by (app) (count_over_time({app="foo"}[1m]))) of ({app="foo"}[1m])`,
						Plan: &plan.QueryPlan{
							AST: syntax.MustParseExpr(`variants(sum by (app) (bytes_over_time({app="foo"}[1m])), sum by (app) (count_over_time({app="foo"}[1m]))) of ({app="foo"}[1m])`),
						},
						Start: time.Unix(0, 0),
						End:   time.Unix(60, 0),
					},
				},
			},
			promql.Vector{
				promql.Sample{T: 60 * 1000, F: 120, Metric: labels.FromStrings(constants.VariantLabel, "0", "app", "foo")},
				promql.Sample{T: 60 * 1000, F: 120, Metric: labels.FromStrings(constants.VariantLabel, "1", "app", "foo")},
			},
		},
		{
			`variants(bytes_over_time({app="foo"}[1m]), count_over_time({app="foo"}[1m])) of ({app="foo"}[1m])`,
			time.Unix(60, 0),
			logproto.BACKWARD,
			0,
			[][]logproto.Series{
				{
					newSeries(testSize, identity, `{app="foo", foo="bar"}`),
					newSeries(testSize, identity, `{app="foo", foo="baz"}`),
				},
			},
			[]SelectSampleParams{
				{
					&logproto.SampleQueryRequest{
						Selector: `variants(bytes_over_time({app="foo"}[1m]), count_over_time({app="foo"}[1m])) of ({app="foo"}[1m])`,
						Plan: &plan.QueryPlan{
							AST: syntax.MustParseExpr(`variants(bytes_over_time({app="foo"}[1m]), count_over_time({app="foo"}[1m])) of ({app="foo"}[1m])`),
						},
						Start: time.Unix(0, 0),
						End:   time.Unix(60, 0),
					},
				},
			},
			promql.Vector{
				promql.Sample{T: 60 * 1000, F: 60, Metric: labels.FromStrings(constants.VariantLabel, "0", "app", "foo", "foo", "bar")},
				promql.Sample{T: 60 * 1000, F: 60, Metric: labels.FromStrings(constants.VariantLabel, "0", "app", "foo", "foo", "baz")},
				promql.Sample{T: 60 * 1000, F: 60, Metric: labels.FromStrings(constants.VariantLabel, "1", "app", "foo", "foo", "bar")},
				promql.Sample{T: 60 * 1000, F: 60, Metric: labels.FromStrings(constants.VariantLabel, "1", "app", "foo", "foo", "baz")},
			},
		},
		{
			`variants(sum by (app) (bytes_over_time({app="foo"}[1m])), count_over_time({app="foo"}[1m])) of ({app="foo"}[1m])`,
			time.Unix(60, 0),
			logproto.BACKWARD,
			0,
			[][]logproto.Series{
				{
					newSeries(testSize, identity, `{app="foo", foo="bar"}`),
					newSeries(testSize, identity, `{app="foo", foo="baz"}`),
				},
			},
			[]SelectSampleParams{
				{
					&logproto.SampleQueryRequest{
						Selector: `variants(sum by (app) (bytes_over_time({app="foo"}[1m])), count_over_time({app="foo"}[1m])) of ({app="foo"}[1m])`,
						Plan: &plan.QueryPlan{
							AST: syntax.MustParseExpr(`variants(sum by (app) (bytes_over_time({app="foo"}[1m])), count_over_time({app="foo"}[1m])) of ({app="foo"}[1m])`),
						},
						Start: time.Unix(0, 0),
						End:   time.Unix(60, 0),
					},
				},
			},
			promql.Vector{
				promql.Sample{T: 60 * 1000, F: 120, Metric: labels.FromStrings(constants.VariantLabel, "0", "app", "foo")},
				promql.Sample{T: 60 * 1000, F: 60, Metric: labels.FromStrings(constants.VariantLabel, "1", "app", "foo", "foo", "bar")},
				promql.Sample{T: 60 * 1000, F: 60, Metric: labels.FromStrings(constants.VariantLabel, "1", "app", "foo", "foo", "baz")},
			},
		},
	} {
		t.Run(fmt.Sprintf("%s %s", test.qs, test.direction), func(t *testing.T) {
			eng := NewEngine(
				EngineOpts{},
				newQuerierRecorder(t, test.data, test.params),
				customLimits,
				log.NewNopLogger(),
			)

			params, err := NewLiteralParams(
				test.qs,
				test.ts,
				test.ts,
				0,
				0,
				test.direction,
				test.limit,
				nil,
				nil,
			)
			require.NoError(t, err)
			q := eng.Query(params)
			res, err := q.Exec(user.InjectOrgID(context.Background(), "fake"))
			if expectedError, ok := test.expected.(error); ok {
				assert.Equal(t, expectedError.Error(), err.Error())
			} else {
				if err != nil {
					t.Fatal(err)
				}
				assert.Equal(t, test.expected, res.Data)
			}
		})
	}
}

func TestJoinMultiVariantSampleVector(t *testing.T) {
	t.Parallel()

	now := time.Now()
	expr, err := syntax.ParseExpr(`variants(count_over_time({app="foo"}[1m])) of ({app="foo"}[1m])`)
	require.NoError(t, err)

	instantParams := LiteralParams{
		queryExpr: expr,
		limit:     10,
		start:     now,
		end:       now,
		step:      time.Duration(0),
	}

	rangeParams := LiteralParams{
		queryExpr: expr,
		limit:     10,
		start:     now.Add(-time.Hour),
		end:       now,
		step:      30 * time.Second,
	}

	testCases := []struct {
		name             string
		params           Params
		maxSeries        int
		initialVector    promql.Vector
		stepResults      []StepResult
		expectedResult   promql_parser.Value
		expectedWarnings []string
	}{
		{
			name:      "instant query within limits",
			params:    instantParams,
			maxSeries: 3,
			initialVector: promql.Vector{
				{T: 60 * 1000, F: 1, Metric: labels.FromStrings(constants.VariantLabel, "0", "app", "foo")},
				{T: 60 * 1000, F: 2, Metric: labels.FromStrings(constants.VariantLabel, "0", "app", "bar")},
			},
			expectedResult: promql.Vector{
				{T: 60 * 1000, F: 2, Metric: labels.FromStrings(constants.VariantLabel, "0", "app", "bar")}, //bar comes first alphabetically
				{T: 60 * 1000, F: 1, Metric: labels.FromStrings(constants.VariantLabel, "0", "app", "foo")},
			},
		},
		{
			name:      "instant query where each variant falls within limits, but aggregate is over limit",
			params:    instantParams,
			maxSeries: 3,
			initialVector: promql.Vector{
				{T: 60 * 1000, F: 1, Metric: labels.FromStrings(constants.VariantLabel, "0", "app", "foo")},
				{T: 60 * 1000, F: 2, Metric: labels.FromStrings(constants.VariantLabel, "0", "app", "bar")},
				{T: 60 * 1000, F: 1, Metric: labels.FromStrings(constants.VariantLabel, "1", "app", "foo")},
				{T: 60 * 1000, F: 2, Metric: labels.FromStrings(constants.VariantLabel, "1", "app", "bar")},
			},
			expectedResult: promql.Vector{
				{T: 60 * 1000, F: 2, Metric: labels.FromStrings(constants.VariantLabel, "0", "app", "bar")}, //bar comes first alphabetically
				{T: 60 * 1000, F: 1, Metric: labels.FromStrings(constants.VariantLabel, "0", "app", "foo")},
				{T: 60 * 1000, F: 2, Metric: labels.FromStrings(constants.VariantLabel, "1", "app", "bar")},
				{T: 60 * 1000, F: 1, Metric: labels.FromStrings(constants.VariantLabel, "1", "app", "foo")},
			},
		},
		{
			name:      "instant query with a variant over the limits",
			params:    instantParams,
			maxSeries: 3,
			initialVector: promql.Vector{
				{T: 60 * 1000, F: 1, Metric: labels.FromStrings(constants.VariantLabel, "0", "app", "foo")},
				{T: 60 * 1000, F: 2, Metric: labels.FromStrings(constants.VariantLabel, "0", "app", "bar")},
				{T: 60 * 1000, F: 3, Metric: labels.FromStrings(constants.VariantLabel, "0", "app", "baz")},
				{T: 60 * 1000, F: 4, Metric: labels.FromStrings(constants.VariantLabel, "0", "app", "qux")},
				{T: 60 * 1000, F: 1, Metric: labels.FromStrings(constants.VariantLabel, "1", "app", "foo")},
				{T: 60 * 1000, F: 2, Metric: labels.FromStrings(constants.VariantLabel, "1", "app", "bar")},
			},
			expectedResult: promql.Vector{
				{T: 60 * 1000, F: 2, Metric: labels.FromStrings(constants.VariantLabel, "1", "app", "bar")},
				{T: 60 * 1000, F: 1, Metric: labels.FromStrings(constants.VariantLabel, "1", "app", "foo")},
			},
			expectedWarnings: []string{"maximum of series (3) reached for variant (0)"},
		},
		{
			name:      "range query with multiple steps within limits",
			params:    rangeParams,
			maxSeries: 3,
			initialVector: promql.Vector{
				{T: 60 * 1000, F: 1, Metric: labels.FromStrings(constants.VariantLabel, "0", "app", "foo")},
			},
			stepResults: []StepResult{
				vectorResult(promql.Vector{
					{T: 90 * 1000, F: 2, Metric: labels.FromStrings(constants.VariantLabel, "0", "app", "foo")},
				}),
				vectorResult(promql.Vector{
					{T: 120 * 1000, F: 3, Metric: labels.FromStrings(constants.VariantLabel, "0", "app", "foo")},
				}),
			},
			expectedResult: promql.Matrix{
				promql.Series{
					Metric: labels.FromStrings(constants.VariantLabel, "0", "app", "foo"),
					Floats: []promql.FPoint{
						{T: 60 * 1000, F: 1},
						{T: 90 * 1000, F: 2},
						{T: 120 * 1000, F: 3},
					},
				},
			},
		},
		{
			name:      "range query with multiple steps within limits per variant, but over the limit in aggregate",
			params:    rangeParams,
			maxSeries: 3,
			initialVector: promql.Vector{
				{T: 60 * 1000, F: 1, Metric: labels.FromStrings(constants.VariantLabel, "0", "app", "foo")},
				{T: 60 * 1000, F: 1, Metric: labels.FromStrings(constants.VariantLabel, "1", "job", "bar")},
			},
			stepResults: []StepResult{
				vectorResult(promql.Vector{
					{T: 90 * 1000, F: 2, Metric: labels.FromStrings(constants.VariantLabel, "0", "app", "foo")},
					{T: 90 * 1000, F: 2, Metric: labels.FromStrings(constants.VariantLabel, "1", "job", "bar")},
				}),
				vectorResult(promql.Vector{
					{T: 120 * 1000, F: 3, Metric: labels.FromStrings(constants.VariantLabel, "0", "app", "foo")},
					{T: 120 * 1000, F: 3, Metric: labels.FromStrings(constants.VariantLabel, "1", "job", "bar")},
				}),
				vectorResult(promql.Vector{
					{T: 150 * 1000, F: 4, Metric: labels.FromStrings(constants.VariantLabel, "0", "app", "foo")},
					{T: 150 * 1000, F: 4, Metric: labels.FromStrings(constants.VariantLabel, "1", "job", "bar")},
				}),
			},
			expectedResult: promql.Matrix{
				promql.Series{
					Metric: labels.FromStrings(constants.VariantLabel, "0", "app", "foo"),
					Floats: []promql.FPoint{
						{T: 60 * 1000, F: 1},
						{T: 90 * 1000, F: 2},
						{T: 120 * 1000, F: 3},
						{T: 150 * 1000, F: 4},
					},
				},
				promql.Series{
					Metric: labels.FromStrings(constants.VariantLabel, "1", "job", "bar"),
					Floats: []promql.FPoint{
						{T: 60 * 1000, F: 1},
						{T: 90 * 1000, F: 2},
						{T: 120 * 1000, F: 3},
						{T: 150 * 1000, F: 4},
					},
				},
			},
		},
		{
			name:      "range query with a variant over the limit",
			params:    rangeParams,
			maxSeries: 3,
			initialVector: promql.Vector{
				{T: 60 * 1000, F: 1, Metric: labels.FromStrings(constants.VariantLabel, "0", "app", "foo")},
				{T: 60 * 1000, F: 1, Metric: labels.FromStrings(constants.VariantLabel, "1", "job", "foo")},
				{T: 60 * 1000, F: 1, Metric: labels.FromStrings(constants.VariantLabel, "1", "job", "bar")},
				{T: 60 * 1000, F: 1, Metric: labels.FromStrings(constants.VariantLabel, "1", "job", "baz")},
				{T: 60 * 1000, F: 1, Metric: labels.FromStrings(constants.VariantLabel, "1", "job", "qux")},
			},
			stepResults: []StepResult{
				vectorResult(promql.Vector{
					{T: 90 * 1000, F: 2, Metric: labels.FromStrings(constants.VariantLabel, "0", "app", "foo")},
					{T: 90 * 1000, F: 2, Metric: labels.FromStrings(constants.VariantLabel, "1", "job", "foo")},
					{T: 90 * 1000, F: 2, Metric: labels.FromStrings(constants.VariantLabel, "1", "job", "bar")},
					{T: 90 * 1000, F: 2, Metric: labels.FromStrings(constants.VariantLabel, "1", "job", "baz")},
					{T: 90 * 1000, F: 2, Metric: labels.FromStrings(constants.VariantLabel, "1", "job", "qux")},
				}),
				vectorResult(promql.Vector{
					{T: 120 * 1000, F: 3, Metric: labels.FromStrings(constants.VariantLabel, "0", "app", "foo")},
					{T: 120 * 1000, F: 3, Metric: labels.FromStrings(constants.VariantLabel, "1", "job", "foo")},
					{T: 120 * 1000, F: 3, Metric: labels.FromStrings(constants.VariantLabel, "1", "job", "bar")},
					{T: 120 * 1000, F: 3, Metric: labels.FromStrings(constants.VariantLabel, "1", "job", "baz")},
					{T: 120 * 1000, F: 3, Metric: labels.FromStrings(constants.VariantLabel, "1", "job", "qux")},
				}),
			},
			expectedResult: promql.Matrix{
				promql.Series{
					Metric: labels.FromStrings(constants.VariantLabel, "0", "app", "foo"),
					Floats: []promql.FPoint{
						{T: 60 * 1000, F: 1},
						{T: 90 * 1000, F: 2},
						{T: 120 * 1000, F: 3},
					},
				},
			},
			expectedWarnings: []string{"maximum of series (3) reached for variant (1)"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			q := &query{
				params: tc.params,
			}

			mockEvaluator := &mockStepEvaluator{
				results: tc.stepResults,
				t:       t,
			}

			metadataCtx, ctx := metadata.NewContext(context.Background())
			result, err := q.JoinMultiVariantSampleVector(ctx, true, vectorResult(tc.initialVector), mockEvaluator, tc.maxSeries)
			require.NoError(t, err)
			require.Equal(t, tc.expectedResult, result)

			if tc.expectedWarnings != nil {
				require.Equal(t, tc.expectedWarnings, metadataCtx.Warnings())
			}
		})
	}
}

// vectorResult is a helper that creates a StepResult from a vector
func vectorResult(v promql.Vector) StepResult {
	return &storeSampleResult{vector: v}
}

// mockStepEvaluator is a mock implementation of StepEvaluator for testing
type mockStepEvaluator struct {
	results []StepResult
	current int
	err     error
	t       *testing.T
}

func (m *mockStepEvaluator) Next() (bool, int64, StepResult) {
	if m.current >= len(m.results) {
		return false, 0, nil
	}
	result := m.results[m.current]
	m.current++
	return true, 0, result
}

func (m *mockStepEvaluator) Error() error {
	return m.err
}

func (m *mockStepEvaluator) Close() error {
	return nil
}

func (m *mockStepEvaluator) Explain(_ Node) {
}

// storeSampleResult implements StepResult for testing
type storeSampleResult struct {
	vector promql.Vector
}

func (s *storeSampleResult) SampleVector() promql.Vector {
	return s.vector
}

func (s *storeSampleResult) QuantileSketchVec() ProbabilisticQuantileVector {
	return ProbabilisticQuantileVector{}
}

func (s *storeSampleResult) CountMinSketchVec() CountMinSketchVector {
	return CountMinSketchVector{}
}

func TestEngine_Variants_RangeQuery(t *testing.T) {
	t.Parallel()

	// Create a custom fakeLimits to enable multi-variant queries
	customLimits := &fakeLimits{
		maxSeries:               math.MaxInt32,
		timeout:                 time.Hour,
		multiVariantQueryEnable: true,
	}

	for _, test := range []struct {
		qs        string
		start     time.Time
		end       time.Time
		step      time.Duration
		interval  time.Duration
		direction logproto.Direction
		limit     uint32

		// an array of streams per SelectParams will be returned by the querier.
		// This is to cover logql that requires multiple queries.
		data   interface{}
		params interface{}

		expected promql_parser.Value
	}{
		{
			`variants(bytes_over_time({app="foo"}[1m]), count_over_time({app="foo"}[1m])) of ({app="foo"}[1m])`,
			time.Unix(60, 0), time.Unix(120, 0), time.Minute, 0, logproto.FORWARD, 10,
			[][]logproto.Series{
				{newSeries(testSize, identity, `{app="foo"}`)},
			},
			[]SelectSampleParams{
				{
					&logproto.SampleQueryRequest{
						Selector: `variants(bytes_over_time({app="foo"}[1m]), count_over_time({app="foo"}[1m])) of ({app="foo"}[1m])`,
						Plan: &plan.QueryPlan{
							AST: syntax.MustParseExpr(`variants(bytes_over_time({app="foo"}[1m]), count_over_time({app="foo"}[1m])) of ({app="foo"}[1m])`),
						},
						Start: time.Unix(0, 0),
						End:   time.Unix(120, 0),
					},
				},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.FromStrings(constants.VariantLabel, "0", "app", "foo"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 60}, {T: 120 * 1000, F: 60}},
				},
				promql.Series{
					Metric: labels.FromStrings(constants.VariantLabel, "1", "app", "foo"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 60}, {T: 120 * 1000, F: 60}},
				},
			},
		},
		{
			`variants(sum by (app) (bytes_over_time({app="foo"}[1m])), sum by (app) (count_over_time({app="foo"}[1m]))) of ({app="foo"}[1m])`,
			time.Unix(60, 0), time.Unix(120, 0), time.Minute, 0, logproto.BACKWARD, 10,
			[][]logproto.Series{
				{
					newSeries(testSize, identity, `{app="foo", foo="bar"}`),
					newSeries(testSize, identity, `{app="foo", foo="baz"}`),
				},
			},
			[]SelectSampleParams{
				{
					&logproto.SampleQueryRequest{
						Selector: `variants(sum by (app) (bytes_over_time({app="foo"}[1m])), sum by (app) (count_over_time({app="foo"}[1m]))) of ({app="foo"}[1m])`,
						Plan: &plan.QueryPlan{
							AST: syntax.MustParseExpr(`variants(sum by (app) (bytes_over_time({app="foo"}[1m])), sum by (app) (count_over_time({app="foo"}[1m]))) of ({app="foo"}[1m])`),
						},
						Start: time.Unix(0, 0),
						End:   time.Unix(60, 0),
					},
				},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.FromStrings(constants.VariantLabel, "0", "app", "foo"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 120}, {T: 120 * 1000, F: 120}},
				},
				promql.Series{
					Metric: labels.FromStrings(constants.VariantLabel, "1", "app", "foo"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 120}, {T: 120 * 1000, F: 120}},
				},
			},
		},
		{
			`variants(bytes_over_time({app="foo"}[1m]), count_over_time({app="foo"}[1m])) of ({app="foo"}[1m])`,
			time.Unix(60, 0), time.Unix(120, 0), time.Minute, 0, logproto.BACKWARD, 10,
			[][]logproto.Series{
				{
					newSeries(testSize, identity, `{app="foo", foo="bar"}`),
					newSeries(testSize, identity, `{app="foo", foo="baz"}`),
				},
			},
			[]SelectSampleParams{
				{
					&logproto.SampleQueryRequest{
						Selector: `variants(bytes_over_time({app="foo"}[1m]), count_over_time({app="foo"}[1m])) of ({app="foo"}[1m])`,
						Plan: &plan.QueryPlan{
							AST: syntax.MustParseExpr(`variants(bytes_over_time({app="foo"}[1m]), count_over_time({app="foo"}[1m])) of ({app="foo"}[1m])`),
						},
						Start: time.Unix(0, 0),
						End:   time.Unix(60, 0),
					},
				},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.FromStrings(constants.VariantLabel, "0", "app", "foo", "foo", "bar"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 60}, {T: 120 * 1000, F: 60}},
				},
				promql.Series{
					Metric: labels.FromStrings(constants.VariantLabel, "0", "app", "foo", "foo", "baz"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 60}, {T: 120 * 1000, F: 60}},
				},
				promql.Series{
					Metric: labels.FromStrings(constants.VariantLabel, "1", "app", "foo", "foo", "bar"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 60}, {T: 120 * 1000, F: 60}},
				},
				promql.Series{
					Metric: labels.FromStrings(constants.VariantLabel, "1", "app", "foo", "foo", "baz"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 60}, {T: 120 * 1000, F: 60}},
				},
			},
		},
		{
			`variants(sum by (app) (bytes_over_time({app="foo"}[1m])), count_over_time({app="foo"}[1m])) of ({app="foo"}[1m])`,
			time.Unix(60, 0), time.Unix(120, 0), time.Minute, 0, logproto.BACKWARD, 10,
			[][]logproto.Series{
				{
					newSeries(testSize, identity, `{app="foo", foo="bar"}`),
					newSeries(testSize, identity, `{app="foo", foo="baz"}`),
				},
			},
			[]SelectSampleParams{
				{
					&logproto.SampleQueryRequest{
						Selector: `variants(sum by (app) (bytes_over_time({app="foo"}[1m])), count_over_time({app="foo"}[1m])) of ({app="foo"}[1m])`,
						Plan: &plan.QueryPlan{
							AST: syntax.MustParseExpr(`variants(sum by (app) (bytes_over_time({app="foo"}[1m])), count_over_time({app="foo"}[1m])) of ({app="foo"}[1m])`),
						},
						Start: time.Unix(0, 0),
						End:   time.Unix(60, 0),
					},
				},
			},
			promql.Matrix{
				promql.Series{
					Metric: labels.FromStrings(constants.VariantLabel, "0", "app", "foo"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 120}, {T: 120 * 1000, F: 120}},
				},
				promql.Series{
					Metric: labels.FromStrings(constants.VariantLabel, "1", "app", "foo", "foo", "bar"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 60}, {T: 120 * 1000, F: 60}},
				},
				promql.Series{
					Metric: labels.FromStrings(constants.VariantLabel, "1", "app", "foo", "foo", "baz"),
					Floats: []promql.FPoint{{T: 60 * 1000, F: 60}, {T: 120 * 1000, F: 60}},
				},
			},
		},
	} {
		t.Run(fmt.Sprintf("%s %s", test.qs, test.direction), func(t *testing.T) {
			t.Parallel()

			eng := NewEngine(
				EngineOpts{},
				newQuerierRecorder(t, test.data, test.params),
				customLimits,
				log.NewNopLogger(),
			)

			params, err := NewLiteralParams(
				test.qs,
				test.start,
				test.end,
				test.step,
				test.interval,
				test.direction,
				test.limit,
				nil,
				nil,
			)
			require.NoError(t, err)
			q := eng.Query(params)
			res, err := q.Exec(user.InjectOrgID(context.Background(), "fake"))
			if err != nil {
				t.Fatal(err)
			}
			assert.Equal(t, test.expected, res.Data)
		})
	}
}

type statsQuerier struct{}

func (statsQuerier) SelectLogs(ctx context.Context, _ SelectLogParams) (iter.EntryIterator, error) {
	st := stats.FromContext(ctx)
	st.AddDecompressedBytes(1)
	return iter.NoopEntryIterator, nil
}

func (statsQuerier) SelectSamples(ctx context.Context, _ SelectSampleParams) (iter.SampleIterator, error) {
	st := stats.FromContext(ctx)
	st.AddDecompressedBytes(1)
	return iter.NoopSampleIterator, nil
}

func TestEngine_Stats(t *testing.T) {
	eng := NewEngine(EngineOpts{}, &statsQuerier{}, NoLimits, log.NewNopLogger())

	queueTime := 2 * time.Nanosecond

	params, err := NewLiteralParams(`{foo="bar"}`, time.Now(), time.Now(), 0, 0, logproto.FORWARD, 1000, nil, nil)
	require.NoError(t, err)
	q := eng.Query(params)

	ctx := context.WithValue(context.Background(), httpreq.QueryQueueTimeHTTPHeader, queueTime)
	r, err := q.Exec(user.InjectOrgID(ctx, "fake"))
	require.NoError(t, err)
	require.Equal(t, int64(1), r.Statistics.TotalDecompressedBytes())
	require.Equal(t, queueTime.Seconds(), r.Statistics.Summary.QueueTime)
}

type metaQuerier struct{}

func (metaQuerier) SelectLogs(ctx context.Context, _ SelectLogParams) (iter.EntryIterator, error) {
	_ = metadata.JoinHeaders(ctx, []*definitions.PrometheusResponseHeader{
		{
			Name:   "Header",
			Values: []string{"value"},
		},
	})
	return iter.NoopEntryIterator, nil
}

func (metaQuerier) SelectSamples(
	ctx context.Context,
	_ SelectSampleParams,
) (iter.SampleIterator, error) {
	_ = metadata.JoinHeaders(ctx, []*definitions.PrometheusResponseHeader{
		{Name: "Header", Values: []string{"value"}},
	})
	return iter.NoopSampleIterator, nil
}

func TestEngine_Metadata(t *testing.T) {
	eng := NewEngine(EngineOpts{}, &metaQuerier{}, NoLimits, log.NewNopLogger())

	params, err := NewLiteralParams(`{foo="bar"}`, time.Now(), time.Now(), 0, 0, logproto.BACKWARD, 1000, nil, nil)
	require.NoError(t, err)
	q := eng.Query(params)

	r, err := q.Exec(user.InjectOrgID(context.Background(), "fake"))
	require.NoError(t, err)
	require.Equal(t, []*definitions.PrometheusResponseHeader{
		{Name: "Header", Values: []string{"value"}},
	}, r.Headers)
}

func TestEngine_LogsInstantQuery_Vector(t *testing.T) {
	eng := NewEngine(EngineOpts{}, &statsQuerier{}, NoLimits, log.NewNopLogger())
	now := time.Now()
	queueTime := 2 * time.Nanosecond
	logqlVector := `vector(5)`

	params, err := NewLiteralParams(logqlVector, now, now, 0, time.Second*30, logproto.BACKWARD, 1000, nil, nil)
	require.NoError(t, err)
	q := eng.Query(params)
	ctx := context.WithValue(context.Background(), httpreq.QueryQueueTimeHTTPHeader, queueTime)
	_, err = q.Exec(user.InjectOrgID(ctx, "fake"))

	require.NoError(t, err)

	qry, ok := q.(*query)
	require.Equal(t, ok, true)
	vectorExpr := syntax.NewVectorExpr("5")

	data, err := qry.evalSample(ctx, vectorExpr)
	require.NoError(t, err)
	result, ok := data.(promql.Vector)
	require.Equal(t, ok, true)
	require.Equal(t, result[0].F, float64(5))
	require.Equal(t, result[0].T, now.UnixNano()/int64(time.Millisecond))
}

type errorIteratorQuerier struct {
	samples func() []iter.SampleIterator
	entries func() []iter.EntryIterator
}

func (e errorIteratorQuerier) SelectLogs(_ context.Context, p SelectLogParams) (iter.EntryIterator, error) {
	return iter.NewSortEntryIterator(e.entries(), p.Direction), nil
}

func (e errorIteratorQuerier) SelectSamples(_ context.Context, _ SelectSampleParams) (iter.SampleIterator, error) {
	return iter.NewSortSampleIterator(e.samples()), nil
}

func TestMultiVariantQueries_Limits(t *testing.T) {
	variantQuery := `variants(bytes_over_time({app="foo"}[1m]), count_over_time({app="foo"}[1m])) of ({app="foo"}[1m])`
	testTime := time.Unix(60, 0)

	t.Run("disabled", func(t *testing.T) {
		// Create limits with multi-variant queries disabled
		limitsDisabled := &fakeLimits{
			maxSeries:               math.MaxInt32,
			timeout:                 time.Hour,
			multiVariantQueryEnable: false,
		}

		eng := NewEngine(EngineOpts{}, &statsQuerier{}, limitsDisabled, log.NewNopLogger())
		params, err := NewLiteralParams(
			variantQuery,
			testTime,
			testTime,
			0,
			0,
			logproto.BACKWARD,
			0,
			nil,
			nil,
		)
		require.NoError(t, err)

		// Query should fail with variants disabled error
		q := eng.Query(params)
		_, err = q.Exec(user.InjectOrgID(context.Background(), "fake"))
		require.ErrorIs(t, err, logqlmodel.ErrVariantsDisabled)
	})

	t.Run("enabled", func(t *testing.T) {
		// Create limits with multi-variant queries enabled
		limitsEnabled := &fakeLimits{
			maxSeries:               math.MaxInt32,
			timeout:                 time.Hour,
			multiVariantQueryEnable: true,
		}

		// Use a fake series for the query
		series := []logproto.Series{
			{
				Labels: `{app="foo"}`,
				Samples: []logproto.Sample{
					{Timestamp: testTime.UnixNano(), Hash: 1, Value: 5},
				},
			},
		}

		plan := &plan.QueryPlan{
			AST: syntax.MustParseExpr(variantQuery),
		}

		sampleReq := &logproto.SampleQueryRequest{
			Start:    time.Unix(0, 0),
			End:      testTime,
			Selector: variantQuery,
			Plan:     plan,
		}

		data := [][]logproto.Series{series}
		params := []SelectSampleParams{{sampleReq}}

		eng := NewEngine(EngineOpts{}, newQuerierRecorder(t, data, params), limitsEnabled, log.NewNopLogger())
		queryParams, err := NewLiteralParams(
			variantQuery,
			testTime,
			testTime,
			0,
			0,
			logproto.BACKWARD,
			0,
			nil,
			nil,
		)
		require.NoError(t, err)

		// Query should succeed with multi-variant enabled
		q := eng.Query(queryParams)
		result, err := q.Exec(user.InjectOrgID(context.Background(), "fake"))
		require.NoError(t, err)
		require.NotNil(t, result.Data)
	})
}

func TestStepEvaluator_Error(t *testing.T) {
	tests := []struct {
		name    string
		qs      string
		querier Querier
		err     error
	}{
		{
			"rangeAggEvaluator",
			`count_over_time({app="foo"}[1m])`,
			&errorIteratorQuerier{
				samples: func() []iter.SampleIterator {
					return []iter.SampleIterator{
						iter.NewSeriesIterator(newSeries(testSize, identity, `{app="foo"}`)),
						iter.ErrorSampleIterator,
					}
				},
			},
			ErrMock,
		},
		{
			"stream",
			`{app="foo"}`,
			&errorIteratorQuerier{
				entries: func() []iter.EntryIterator {
					return []iter.EntryIterator{
						iter.NewStreamIterator(newStream(testSize, identity, `{app="foo"}`)),
						iter.ErrorEntryIterator,
					}
				},
			},
			ErrMock,
		},
		{
			"binOpStepEvaluator",
			`count_over_time({app="foo"}[1m]) / count_over_time({app="foo"}[1m])`,
			&errorIteratorQuerier{
				samples: func() []iter.SampleIterator {
					return []iter.SampleIterator{
						iter.NewSeriesIterator(newSeries(testSize, identity, `{app="foo"}`)),
						iter.ErrorSampleIterator,
					}
				},
			},
			ErrMockMultiple,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			eng := NewEngine(EngineOpts{}, tc.querier, NoLimits, log.NewNopLogger())

			params, err := NewLiteralParams(tc.qs, time.Unix(0, 0), time.Unix(180, 0), 1*time.Second, 0, logproto.BACKWARD, 1, nil, nil)
			require.NoError(t, err)
			q := eng.Query(params)
			_, err = q.Exec(user.InjectOrgID(context.Background(), "fake"))
			require.Equal(t, tc.err, err)
		})
	}
}

func TestEngine_MaxSeries(t *testing.T) {
	eng := NewEngine(EngineOpts{}, getLocalQuerier(100000), &fakeLimits{maxSeries: 1}, log.NewNopLogger())

	for _, test := range []struct {
		qs             string
		direction      logproto.Direction
		expectLimitErr bool
	}{
		{`topk(1,rate(({app=~"foo|bar"})[1m]))`, logproto.FORWARD, true},
		{`{app="foo"}`, logproto.FORWARD, false},
		{`{app="bar"} |= "foo" |~ ".+bar"`, logproto.BACKWARD, false},
		{`rate({app="foo"} |~".+bar" [1m])`, logproto.BACKWARD, true},
		{`rate({app="foo"}[30s])`, logproto.FORWARD, true},
		{`count_over_time({app="foo|bar"} |~".+bar" [1m])`, logproto.BACKWARD, true},
		{`avg(count_over_time({app=~"foo|bar"} |~".+bar" [1m]))`, logproto.FORWARD, false},
	} {
		t.Run(test.qs, func(t *testing.T) {
			params, err := NewLiteralParams(test.qs, time.Unix(0, 0), time.Unix(100000, 0), 60*time.Second, 0, test.direction, 1000, nil, nil)
			require.NoError(t, err)
			q := eng.Query(params)
			_, err = q.Exec(user.InjectOrgID(context.Background(), "fake"))
			if test.expectLimitErr {
				require.NotNil(t, err)
				require.True(t, errors.Is(err, logqlmodel.ErrLimit))
			} else {
				require.Nil(t, err)
			}
		})
	}
}

func TestEngine_MaxRangeInterval(t *testing.T) {
	eng := NewEngine(EngineOpts{}, getLocalQuerier(100000), &fakeLimits{rangeLimit: 24 * time.Hour, maxSeries: 100000}, log.NewNopLogger())

	for _, test := range []struct {
		qs             string
		direction      logproto.Direction
		expectLimitErr bool
	}{
		{`topk(1,rate(({app=~"foo|bar"})[2d]))`, logproto.FORWARD, true},
		{`topk(1,rate(({app=~"foo|bar"})[1d]))`, logproto.FORWARD, false},
		{`topk(1,rate({app=~"foo|bar"}[12h]) / (rate({app="baz"}[23h]) + rate({app="fiz"}[25h])))`, logproto.FORWARD, true},
	} {
		t.Run(test.qs, func(t *testing.T) {
			params, err := NewLiteralParams(test.qs, time.Unix(0, 0), time.Unix(100000, 0), 60*time.Second, 0, test.direction, 1000, nil, nil)
			require.NoError(t, err)
			q := eng.Query(params)

			_, err = q.Exec(user.InjectOrgID(context.Background(), "fake"))
			if test.expectLimitErr {
				require.Error(t, err)
				require.ErrorIs(t, err, logqlmodel.ErrIntervalLimit)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// go test -mod=vendor ./pkg/logql/ -bench=.  -benchmem -memprofile memprofile.out -cpuprofile cpuprofile.out
func BenchmarkRangeQuery100000(b *testing.B) {
	benchmarkRangeQuery(int64(100000), b)
}

func BenchmarkRangeQuery200000(b *testing.B) {
	benchmarkRangeQuery(int64(200000), b)
}

func BenchmarkRangeQuery500000(b *testing.B) {
	benchmarkRangeQuery(int64(500000), b)
}

func BenchmarkRangeQuery1000000(b *testing.B) {
	benchmarkRangeQuery(int64(1000000), b)
}

var result promql_parser.Value

func benchmarkRangeQuery(testsize int64, b *testing.B) {
	b.ReportAllocs()
	eng := NewEngine(EngineOpts{}, getLocalQuerier(testsize), NoLimits, log.NewNopLogger())
	start := time.Unix(0, 0)
	end := time.Unix(testsize, 0)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, test := range []struct {
			qs        string
			direction logproto.Direction
		}{
			{`{app="foo"}`, logproto.FORWARD},
			{`{app="bar"} |= "foo" |~ ".+bar"`, logproto.BACKWARD},
			{`rate({app="foo"} |~".+bar" [1m])`, logproto.BACKWARD},
			{`rate({app="foo"}[30s])`, logproto.FORWARD},
			{`count_over_time({app="foo"} |~".+bar" [1m])`, logproto.BACKWARD},
			{`count_over_time(({app="foo"} |~".+bar")[5m])`, logproto.BACKWARD},
			{`avg(count_over_time({app=~"foo|bar"} |~".+bar" [1m]))`, logproto.FORWARD},
			{`min(rate({app=~"foo|bar"} |~".+bar" [1m]))`, logproto.FORWARD},
			{`max by (app) (rate({app=~"foo|bar"} |~".+bar" [1m]))`, logproto.FORWARD},
			{`max(rate({app=~"foo|bar"} |~".+bar" [1m]))`, logproto.FORWARD},
			{`sum(rate({app=~"foo|bar"} |~".+bar" [1m]))`, logproto.FORWARD},
			{`sum(count_over_time({app=~"foo|bar"} |~".+bar" [1m])) by (app)`, logproto.FORWARD},
			{`count(count_over_time({app=~"foo|bar"} |~".+bar" [1m])) without (app)`, logproto.FORWARD},
			{`stdvar without (app) (count_over_time(({app=~"foo|bar"} |~".+bar")[1m])) `, logproto.FORWARD},
			{`stddev(count_over_time(({app=~"foo|bar"} |~".+bar")[1m])) `, logproto.FORWARD},
			{`rate(({app=~"foo|bar"} |~".+bar")[1m])`, logproto.FORWARD},
			{`topk(2,rate(({app=~"foo|bar"} |~".+bar")[1m]))`, logproto.FORWARD},
			{`topk(1,rate(({app=~"foo|bar"} |~".+bar")[1m]))`, logproto.FORWARD},
			{`topk(1,rate(({app=~"foo|bar"} |~".+bar")[1m])) by (app)`, logproto.FORWARD},
			{`bottomk(2,rate(({app=~"foo|bar"} |~".+bar")[1m]))`, logproto.FORWARD},
			{`bottomk(3,rate(({app=~"foo|bar"} |~".+bar")[1m])) without (app)`, logproto.FORWARD},
		} {
			params, err := NewLiteralParams(test.qs, start, end, 60*time.Second, 0, logproto.BACKWARD, 1000, nil, nil)
			require.NoError(b, err)
			q := eng.Query(params)

			res, err := q.Exec(user.InjectOrgID(context.Background(), "fake"))
			if err != nil {
				b.Fatal(err)
			}
			result = res.Data
			if result == nil {
				b.Fatal("unexpected nil result")
			}
		}
	}
}

// TestHashingStability tests logging stability between engine and RecordRangeAndInstantQueryMetrics methods.
func TestHashingStability(t *testing.T) {
	ctx := user.InjectOrgID(context.Background(), "fake")
	params := LiteralParams{
		start:     time.Unix(0, 0),
		end:       time.Unix(5, 0),
		step:      60 * time.Second,
		direction: logproto.FORWARD,
		limit:     1000,
	}

	queryWithEngine := func() string {
		buf := bytes.NewBufferString("")
		logger := log.NewLogfmtLogger(buf)
		eng := NewEngine(EngineOpts{LogExecutingQuery: true}, getLocalQuerier(4), NoLimits, logger)

		parsed, err := syntax.ParseExpr(params.QueryString())
		require.NoError(t, err)
		params.queryExpr = parsed

		query := eng.Query(params)
		_, err = query.Exec(ctx)
		require.NoError(t, err)
		return buf.String()
	}

	queryDirectly := func() string {
		statsResult := stats.Result{
			Summary: stats.Summary{
				BytesProcessedPerSecond: 100000,
				QueueTime:               0.000000002,
				ExecTime:                25.25,
				TotalBytesProcessed:     100000,
				TotalEntriesReturned:    10,
			},
		}
		buf := bytes.NewBufferString("")
		logger := log.NewLogfmtLogger(buf)
		RecordRangeAndInstantQueryMetrics(ctx, logger, params, "200", statsResult, logqlmodel.Streams{logproto.Stream{Entries: make([]logproto.Entry, 10)}})
		return buf.String()
	}

	for _, test := range []struct {
		qs string
	}{
		{`sum by(query_hash) (count_over_time({app="myapp",env="myenv"} |= "error" |= "metrics.go" | logfmt [10s]))`},
		{`sum (count_over_time({app="myapp",env="myenv"} |= "error" |= "metrics.go" | logfmt [10s])) by(query_hash)`},
	} {
		params.queryString = test.qs
		expectedQueryHash := util.HashedQuery(test.qs)

		// check that both places will end up having the same query hash, even though they're emitting different log lines.
		withEngine := queryWithEngine()
		require.Contains(t, withEngine, fmt.Sprintf("query_hash=%d", expectedQueryHash))
		require.Contains(t, withEngine, "step=1m0s")

		directly := queryDirectly()
		require.Contains(t, directly, fmt.Sprintf("query_hash=%d", expectedQueryHash))
		require.Contains(t, directly, "length=5s")
		require.Contains(t, directly, "latency=slow")
	}
}

func TestUnexpectedEmptyResults(t *testing.T) {
	ctx := user.InjectOrgID(context.Background(), "fake")

	mock := &mockEvaluatorFactory{
		SampleEvaluatorFunc(
			func(context.Context, SampleEvaluatorFactory, syntax.SampleExpr, Params) (StepEvaluator, error) {
				return EmptyEvaluator[SampleVector]{value: nil}, nil
			},
		),
		VariantsEvaluatorFunc(
			func(context.Context, syntax.VariantsExpr, Params) (StepEvaluator, error) {
				return EmptyEvaluator[SampleVector]{value: nil}, nil
			},
		),
	}

	eng := NewEngine(EngineOpts{}, nil, NoLimits, log.NewNopLogger())
	params, err := NewLiteralParams(`first_over_time({a=~".+"} | logfmt | unwrap value [1s])`, time.Now(), time.Now(), 0, 0, logproto.BACKWARD, 0, nil, nil)
	require.NoError(t, err)
	q := eng.Query(params).(*query)
	q.evaluator = mock

	_, err = q.Exec(ctx)
	require.Error(t, err)
}

type mockEvaluatorFactory struct {
	sampleEvalFunc  SampleEvaluatorFunc
	variantEvalFunc VariantsEvaluatorFunc
}

func (m *mockEvaluatorFactory) NewStepEvaluator(ctx context.Context, nextEvaluatorFactory SampleEvaluatorFactory, expr syntax.SampleExpr, p Params) (StepEvaluator, error) {
	if m.sampleEvalFunc != nil {
		return m.sampleEvalFunc(ctx, nextEvaluatorFactory, expr, p)
	}
	return nil, errors.New("unimplemented mock SampleEvaluatorFactory")
}

func (m *mockEvaluatorFactory) NewVariantsStepEvaluator(ctx context.Context, expr syntax.VariantsExpr, p Params) (StepEvaluator, error) {
	if m.variantEvalFunc != nil {
		return m.variantEvalFunc(ctx, expr, p)
	}
	return nil, errors.New("unimplemented mock VariantEvaluatorFactory")
}

func (m *mockEvaluatorFactory) NewIterator(context.Context, syntax.LogSelectorExpr, Params) (iter.EntryIterator, error) {
	return nil, errors.New("unimplemented mock EntryEvaluatorFactory")
}

func getLocalQuerier(size int64) Querier {
	return &querierRecorder{
		series: map[string][]logproto.Series{
			"": {
				newSeries(size, identity, `{app="foo"}`),
				newSeries(size, identity, `{app="foo",bar="foo"}`),
				newSeries(size, identity, `{app="foo",bar="bazz"}`),
				newSeries(size, identity, `{app="foo",bar="fuzz"}`),
				newSeries(size, identity, `{app="bar"}`),
				newSeries(size, identity, `{app="bar",bar="foo"}`),
				newSeries(size, identity, `{app="bar",bar="bazz"}`),
				newSeries(size, identity, `{app="bar",bar="fuzz"}`),
			},
		},
		streams: map[string][]logproto.Stream{
			"": {
				newStream(size, identity, `{app="foo"}`),
				newStream(size, identity, `{app="foo",bar="foo"}`),
				newStream(size, identity, `{app="foo",bar="bazz"}`),
				newStream(size, identity, `{app="foo",bar="fuzz"}`),
				newStream(size, identity, `{app="bar"}`),
				newStream(size, identity, `{app="bar",bar="foo"}`),
				newStream(size, identity, `{app="bar",bar="bazz"}`),
				newStream(size, identity, `{app="bar",bar="fuzz"}`),
			},
		},
	}
}

type querierRecorder struct {
	streams map[string][]logproto.Stream
	series  map[string][]logproto.Series
	match   bool
}

func newQuerierRecorder(t *testing.T, data interface{}, params interface{}) *querierRecorder {
	t.Helper()
	streams := map[string][]logproto.Stream{}
	if streamsIn, ok := data.([][]logproto.Stream); ok {
		if paramsIn, ok2 := params.([]SelectLogParams); ok2 {
			for i, p := range paramsIn {
				p.Plan = &plan.QueryPlan{
					AST: syntax.MustParseExpr(p.Selector),
				}
				streams[paramsID(p)] = streamsIn[i]
			}
		}
	}

	series := map[string][]logproto.Series{}
	if seriesIn, ok := data.([][]logproto.Series); ok {
		if paramsIn, ok2 := params.([]SelectSampleParams); ok2 {
			for i, p := range paramsIn {
				expr, ok3 := syntax.MustParseExpr(p.Selector).(syntax.VariantsExpr)
				if ok3 {
					if p.Plan == nil {
						p.Plan = &plan.QueryPlan{
							AST: expr,
						}
					}

					curSeries := seriesIn[i]
					variants := expr.Variants()
					newSeries := make([]logproto.Series, len(curSeries)*len(variants))

					for vi := range variants {
						for si, s := range curSeries {
							lbls, err := promql_parser.ParseMetric(s.Labels)
							if err != nil {
								return nil
							}

							// Add variant label
							b := labels.NewBuilder(lbls)
							b.Set(constants.VariantLabel, fmt.Sprintf("%d", vi))
							lbls = b.Labels()

							// Copy series with new labels
							idx := vi*len(curSeries) + si
							newSeries[idx] = logproto.Series{
								Labels:  lbls.String(),
								Samples: s.Samples,
							}
						}
					}
					series[paramsID(p)] = newSeries
				} else {
					for i, p := range paramsIn {
						if p.Plan == nil {
							p.Plan = &plan.QueryPlan{
								AST: syntax.MustParseExpr(p.Selector),
							}
						}
						series[paramsID(p)] = seriesIn[i]
					}
				}
			}
		}
	}

	return &querierRecorder{
		streams: streams,
		series:  series,
		match:   true,
	}
}

func (q *querierRecorder) SelectLogs(_ context.Context, p SelectLogParams) (iter.EntryIterator, error) {
	if !q.match {
		for _, s := range q.streams {
			return iter.NewStreamsIterator(s, p.Direction), nil
		}
	}
	recordID := paramsID(p)
	streams, ok := q.streams[recordID]
	if !ok {
		return nil, fmt.Errorf("no streams found for id: %s has: %+v", recordID, q.streams)
	}
	return iter.NewStreamsIterator(streams, p.Direction), nil
}

func (q *querierRecorder) SelectSamples(
	_ context.Context,
	p SelectSampleParams,
) (iter.SampleIterator, error) {
	if !q.match {
		for _, s := range q.series {
			return iter.NewMultiSeriesIterator(s), nil
		}
	}
	recordID := paramsID(p)
	if len(q.series) == 0 {
		return iter.NoopSampleIterator, nil
	}
	series, ok := q.series[recordID]
	if !ok {
		return nil, fmt.Errorf("no series found for id: %s has: %+v", recordID, q.series)
	}
	return iter.NewMultiSeriesIterator(series), nil
}

func paramsID(p interface{}) string {
	switch params := p.(type) {
	case SelectLogParams:
	case SelectSampleParams:
		return fmt.Sprintf("%d", params.Plan.Hash())
	}
	b, err := json.Marshal(p)
	if err != nil {
		panic(err)
	}
	return strings.ReplaceAll(string(b), " ", "")
}

type logData struct {
	logproto.Entry
	// nolint
	logproto.Sample
}

type generator func(i int64) logData

func newStream(n int64, f generator, lbsString string) logproto.Stream {
	labels, err := syntax.ParseLabels(lbsString)
	if err != nil {
		panic(err)
	}
	entries := []logproto.Entry{}
	for i := int64(0); i < n; i++ {
		entries = append(entries, f(i).Entry)
	}
	return logproto.Stream{
		Entries: entries,
		Labels:  labels.String(),
	}
}

func newSeries(n int64, f generator, lbsString string) logproto.Series {
	labels, err := syntax.ParseLabels(lbsString)
	if err != nil {
		panic(err)
	}
	samples := []logproto.Sample{}
	for i := int64(0); i < n; i++ {
		samples = append(samples, f(i).Sample)
	}
	return logproto.Series{
		Samples: samples,
		Labels:  labels.String(),
	}
}

func newIntervalStream(n int64, step time.Duration, f generator, labels string) logproto.Stream {
	entries := []logproto.Entry{}
	lastEntry := int64(-100) // Start with a really small value (negative) so we always output the first item
	for i := int64(0); int64(len(entries)) < n; i++ {
		if float64(lastEntry)+step.Seconds() <= float64(i) {
			entries = append(entries, f(i).Entry)
			lastEntry = i
		}
	}
	return logproto.Stream{
		Entries: entries,
		Labels:  labels,
	}
}

func newBackwardStream(n int64, f generator, labels string) logproto.Stream {
	entries := []logproto.Entry{}
	for i := n - 1; i > 0; i-- {
		entries = append(entries, f(i).Entry)
	}
	return logproto.Stream{
		Entries: entries,
		Labels:  labels,
	}
}

func newBackwardIntervalStream(n, expectedResults int64, step time.Duration, f generator, labels string) logproto.Stream {
	entries := []logproto.Entry{}
	lastEntry := int64(100000) // Start with some really big value so that we always output the first item
	for i := n - 1; int64(len(entries)) < expectedResults; i-- {
		if float64(lastEntry)-step.Seconds() >= float64(i) {
			entries = append(entries, f(i).Entry)
			lastEntry = i
		}
	}
	return logproto.Stream{
		Entries: entries,
		Labels:  labels,
	}
}

func identity(i int64) logData {
	return logData{
		Entry: logproto.Entry{
			Timestamp: time.Unix(i, 0),
			Line:      fmt.Sprintf("%d", i),
		},
		Sample: logproto.Sample{
			Timestamp: time.Unix(i, 0).UnixNano(),
			Value:     1.,
			Hash:      uint64(i),
		},
	}
}

// nolint
func factor(j int64, g generator) generator {
	return func(i int64) logData {
		return g(i * j)
	}
}

// nolint
func offset(j int64, g generator) generator {
	return func(i int64) logData {
		return g(i + j)
	}
}

// nolint
func constant(t int64) generator {
	return func(i int64) logData {
		return logData{
			Entry: logproto.Entry{
				Timestamp: time.Unix(t, 0),
				Line:      fmt.Sprintf("%d", i),
			},
			Sample: logproto.Sample{
				Timestamp: time.Unix(t, 0).UnixNano(),
				Hash:      uint64(i),
				Value:     1.0,
			},
		}
	}
}

// nolint
func constantValue(t int64) generator {
	return func(i int64) logData {
		return logData{
			Entry: logproto.Entry{
				Timestamp: time.Unix(i, 0),
				Line:      fmt.Sprintf("%d", i),
			},
			Sample: logproto.Sample{
				Timestamp: time.Unix(i, 0).UnixNano(),
				Hash:      uint64(i),
				Value:     float64(t),
			},
		}
	}
}

// nolint
func incValue(val int64) generator {
	return func(i int64) logData {
		return logData{
			Entry: logproto.Entry{
				Timestamp: time.Unix(i, 0),
				Line:      fmt.Sprintf("%d", i),
			},
			Sample: logproto.Sample{
				Timestamp: time.Unix(i, 0).UnixNano(),
				Hash:      uint64(i),
				Value:     float64(val + i),
			},
		}
	}
}

// nolint
func inverse(g generator) generator {
	return func(i int64) logData {
		return g(-i)
	}
}

func TestJoinSampleVector_LogsDrilldownBehavior(t *testing.T) {
	t.Parallel()

	// Test the JoinSampleVector method directly to test both code paths
	tests := []struct {
		name               string
		queryTags          string
		maxSeries          int
		vectorSize         int // Number of series in the vector to test immediate limit check
		isRangeQuery       bool
		additionalVectors  []int // Additional vectors for range query testing
		expectError        bool
		expectTruncation   bool
		expectedWarningMsg string
	}{
		{
			name:               "Drilldown - immediate limit exceeded in first vector",
			queryTags:          "Source=grafana-lokiexplore-app",
			maxSeries:          2,
			vectorSize:         3,
			isRangeQuery:       false,
			expectError:        false,
			expectTruncation:   true,
			expectedWarningMsg: "maximum number of series (2) reached for a single query; returning partial results",
		},
		{
			name:               "Non-drilldown - immediate limit exceeded in first vector",
			queryTags:          "Source=grafana",
			maxSeries:          2,
			vectorSize:         3,
			isRangeQuery:       false,
			expectError:        true,
			expectTruncation:   false,
			expectedWarningMsg: "",
		},
		{
			name:               "Drilldown - limit NOT exceeded",
			queryTags:          "Source=grafana-lokiexplore-app",
			maxSeries:          5,
			vectorSize:         3,
			isRangeQuery:       false,
			expectError:        false,
			expectTruncation:   false,
			expectedWarningMsg: "",
		},
		{
			name:               "Non-drilldown - limit NOT exceeded",
			queryTags:          "Source=grafana",
			maxSeries:          5,
			vectorSize:         3,
			isRangeQuery:       false,
			expectError:        false,
			expectTruncation:   false,
			expectedWarningMsg: "",
		},
		{
			name:               "Drilldown - range query limit exceeded in second vector",
			queryTags:          "Source=grafana-lokiexplore-app",
			maxSeries:          3,
			vectorSize:         2, // First vector has 2 series
			isRangeQuery:       true,
			additionalVectors:  []int{2}, // Second vector has 2 more unique series (total 4 > limit 3)
			expectError:        false,
			expectTruncation:   true,
			expectedWarningMsg: "maximum number of series (3) reached for a single query; returning partial results",
		},
		{
			name:               "Non-drilldown - range query limit exceeded in second vector",
			queryTags:          "Source=grafana",
			maxSeries:          3,
			vectorSize:         2, // First vector has 2 series
			isRangeQuery:       true,
			additionalVectors:  []int{2}, // Second vector has 2 more unique series (total 4 > limit 3)
			expectError:        true,
			expectTruncation:   false,
			expectedWarningMsg: "",
		},
		{
			name:               "Drilldown - range query limit NOT exceeded across multiple vectors",
			queryTags:          "Source=grafana-lokiexplore-app",
			maxSeries:          5,
			vectorSize:         2, // First vector has 2 series
			isRangeQuery:       true,
			additionalVectors:  []int{2, 1}, // Second has 2, third has 1 (total 5 = limit)
			expectError:        false,
			expectTruncation:   false,
			expectedWarningMsg: "",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Create a mock query with the necessary context
			ctx := context.Background()
			if test.queryTags != "" {
				ctx = httpreq.InjectQueryTags(ctx, test.queryTags)
			}
			_, ctx = metadata.NewContext(ctx)

			// Create mock params - adjust for range vs instant query
			var params *LiteralParams
			if test.isRangeQuery {
				params = &LiteralParams{
					queryString: `rate({app="foo"}[1m])`,
					start:       time.Unix(0, 0),
					end:         time.Unix(120, 0), // Range query: multiple steps
					step:        60 * time.Second,
					interval:    0,
					direction:   logproto.FORWARD,
					limit:       100,
				}
			} else {
				params = &LiteralParams{
					queryString: `rate({app="foo"}[1m])`,
					start:       time.Unix(0, 0),
					end:         time.Unix(60, 0), // Instant query: single step
					step:        30 * time.Second,
					interval:    0,
					direction:   logproto.FORWARD,
					limit:       100,
				}
			}

			q := &query{
				params: params,
			}

			// Create the initial vector with the specified number of series
			vec := make(promql.Vector, test.vectorSize)
			for i := 0; i < test.vectorSize; i++ {
				vec[i] = promql.Sample{
					T:      60 * 1000,
					F:      float64(i + 1),
					Metric: labels.FromStrings("app", fmt.Sprintf("app%d", i)),
				}
			}

			// Create additional vectors for range query testing
			var stepResults []StepResult
			if test.isRangeQuery && len(test.additionalVectors) > 0 {
				seriesOffset := test.vectorSize // Start naming series after the initial vector
				for _, additionalSize := range test.additionalVectors {
					additionalVec := make(promql.Vector, additionalSize)
					for i := 0; i < additionalSize; i++ {
						additionalVec[i] = promql.Sample{
							T:      120 * 1000, // Different timestamp for subsequent steps
							F:      float64(seriesOffset + i + 1),
							Metric: labels.FromStrings("app", fmt.Sprintf("app%d", seriesOffset+i)),
						}
					}
					stepResults = append(stepResults, &storeSampleResult{vector: additionalVec})
					seriesOffset += additionalSize
				}
			}

			// Create a mock step evaluator
			stepEvaluator := &mockStepEvaluator{
				results: stepResults,
				current: 0,
				t:       t,
			}

			// Call JoinSampleVector with context
			result, err := q.JoinSampleVector(ctx, true, &storeSampleResult{vector: vec}, stepEvaluator, test.maxSeries, false)

			if test.expectError {
				require.Error(t, err)
				require.True(t, errors.Is(err, logqlmodel.ErrLimit))
				require.Nil(t, result)
			} else {
				require.NoError(t, err)
				require.NotNil(t, result)

				if test.expectTruncation {
					// Check that the result was truncated to maxSeries
					var actualSeriesCount int
					if vec, ok := result.(promql.Vector); ok {
						// Instant query result
						actualSeriesCount = len(vec)
					} else if matrix, ok := result.(promql.Matrix); ok {
						// Range query result - count unique series
						seriesMap := make(map[string]bool)
						for _, series := range matrix {
							seriesMap[series.Metric.String()] = true
						}
						actualSeriesCount = len(seriesMap)
					} else {
						t.Fatalf("Unexpected result type: %T", result)
					}

					require.LessOrEqual(t, actualSeriesCount, test.maxSeries,
						"Expected result to be truncated to maxSeries (%d), but got %d series",
						test.maxSeries, actualSeriesCount)

					// Check for warning
					meta := metadata.FromContext(ctx)
					warnings := meta.Warnings()
					require.NotEmpty(t, warnings, "Expected warnings but got none")
					require.Contains(t, warnings[0], test.expectedWarningMsg)
				} else {
					// No truncation expected - verify no warnings
					meta := metadata.FromContext(ctx)
					warnings := meta.Warnings()
					if test.expectedWarningMsg == "" {
						require.Empty(t, warnings, "Expected no warnings but got: %v", warnings)
					}
				}
			}
		})
	}
}

func TestHttpreqIsLogsDrilldownRequest(t *testing.T) {
	tests := []struct {
		name      string
		queryTags string
		expected  bool
	}{
		{
			name:      "Valid Logs Drilldown request",
			queryTags: "Source=grafana-lokiexplore-app,Feature=patterns",
			expected:  true,
		},
		{
			name:      "Case insensitive source matching",
			queryTags: "Source=GRAFANA-LOKIEXPLORE-APP,Feature=patterns",
			expected:  true,
		},
		{
			name:      "Different source",
			queryTags: "Source=grafana,Feature=explore",
			expected:  false,
		},
		{
			name:      "No source tag",
			queryTags: "Feature=patterns,User=test",
			expected:  false,
		},
		{
			name:      "Empty query tags",
			queryTags: "",
			expected:  false,
		},
		{
			name:      "Malformed tags",
			queryTags: "invalid_tags_format",
			expected:  false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			if test.queryTags != "" {
				ctx = httpreq.InjectQueryTags(ctx, test.queryTags)
			}

			result := httpreq.IsLogsDrilldownRequest(ctx)
			require.Equal(t, test.expected, result, "Expected %v, got %v for queryTags: %s", test.expected, result, test.queryTags)
		})
	}
}

func TestJoinSampleVector_RangeQueryVectorOverwrite(t *testing.T) {
	t.Parallel()

	// This test covers a vector overwrite issue in range queries for Logs Drilldown.
	// The problem was that after truncating the first vector due to series limit,
	// subsequent steps in the range query can overwrite the truncated vector with larger vectors,
	// causing the final result to exceed the intended series limit.

	ctx := context.Background()
	ctx = httpreq.InjectQueryTags(ctx, "Source=grafana-lokiexplore-app")
	_, ctx = metadata.NewContext(ctx)

	// Create mock params for a range query (multiple steps)
	params := &LiteralParams{
		queryString: `rate({app="foo"}[1m])`,
		start:       time.Unix(0, 0),
		end:         time.Unix(120, 0), // 3 steps with 60s step
		step:        60 * time.Second,
		interval:    0,
		direction:   logproto.FORWARD,
		limit:       100,
	}

	q := &query{
		params: params,
	}

	maxSeries := 2 // Limit to 2 series

	// Create first vector that exceeds the limit (3 series)
	firstVec := make(promql.Vector, 3)
	for i := range 3 {
		firstVec[i] = promql.Sample{
			T:      0 * 1000, // First time step
			F:      float64(i + 1),
			Metric: labels.FromStrings("app", fmt.Sprintf("app%d", i)),
		}
	}

	// Create second vector that also exceeds the limit (4 series)
	// This simulates the case where subsequent steps return even more series
	secondVec := make(promql.Vector, 4)
	for i := range 4 {
		secondVec[i] = promql.Sample{
			T:      60 * 1000, // Second time step
			F:      float64(i + 10),
			Metric: labels.FromStrings("app", fmt.Sprintf("app%d", i)),
		}
	}

	// Create third vector that also exceeds the limit (5 series)
	thirdVec := make(promql.Vector, 5)
	for i := range 5 {
		thirdVec[i] = promql.Sample{
			T:      120 * 1000, // Third time step
			F:      float64(i + 20),
			Metric: labels.FromStrings("app", fmt.Sprintf("app%d", i)),
		}
	}

	// Create a mock step evaluator that returns vectors exceeding the limit on each call
	stepEvaluator := &mockStepEvaluator{
		results: []StepResult{
			&storeSampleResult{vector: secondVec}, // Second call will return 4 series
			&storeSampleResult{vector: thirdVec},  // Third call will return 5 series
		},
		current: 0,
		t:       t,
	}

	// Call JoinSampleVector with the first vector (3 series) and step evaluator
	// that will return even larger vectors in subsequent steps
	result, err := q.JoinSampleVector(ctx, true, &storeSampleResult{vector: firstVec}, stepEvaluator, maxSeries, false)

	require.NoError(t, err)
	require.NotNil(t, result)

	// This test expects the CORRECT behavior: series limit should be respected
	// across all steps of a range query for Logs Drilldown requests
	if matrix, ok := result.(promql.Matrix); ok {
		// Count total unique series across all steps
		seriesMap := make(map[string]bool)
		for _, series := range matrix {
			seriesMap[series.Metric.String()] = true
		}

		// The correct behavior: final result should never exceed maxSeries
		// This assertion will FAIL initially, demonstrating the bug exists
		require.LessOrEqual(t, len(seriesMap), maxSeries,
			"Expected series limit to be respected across all range query steps. "+
				"Found %d series but limit is %d. This indicates the vector overwrite bug exists.",
			len(seriesMap), maxSeries)

		t.Logf("Correct behavior: found %d unique series (within limit of %d)", len(seriesMap), maxSeries)
	} else {
		t.Fatalf("Expected Matrix result, got %T", result)
	}

	// Verify that warnings were still added for the first truncation
	meta := metadata.FromContext(ctx)
	warnings := meta.Warnings()
	require.NotEmpty(t, warnings, "Expected warnings due to series limit exceeded")
	require.Contains(t, warnings[0], "maximum number of series")
}
