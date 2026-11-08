// Package prom runs instant queries and reduces the result to one number.
package prom

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
)

// Querier returns the summed value at time `at`. empty means the result had no samples.
type Querier interface {
	Query(ctx context.Context, expr string, at time.Time) (value float64, empty bool, err error)
}

type client struct {
	api     promv1.API
	timeout time.Duration
}

type tokenRT struct {
	path string
	next http.RoundTripper
}

// RoundTrip re-reads the token file so a rotated projected token keeps working.
func (t tokenRT) RoundTrip(r *http.Request) (*http.Response, error) {
	b, err := os.ReadFile(t.path)
	if err != nil {
		return nil, fmt.Errorf("read bearer token: %w", err)
	}
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(b)))
	return t.next.RoundTrip(r)
}

func New(url, bearerTokenFile string, timeout time.Duration) (Querier, error) {
	rt := api.DefaultRoundTripper
	if bearerTokenFile != "" {
		rt = tokenRT{bearerTokenFile, rt}
	}
	c, err := api.NewClient(api.Config{Address: url, RoundTripper: rt})
	if err != nil {
		return nil, err
	}
	return &client{promv1.NewAPI(c), timeout}, nil
}

func (c *client) Query(ctx context.Context, expr string, at time.Time) (float64, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	v, _, _, err := c.api.Query(ctx, expr, at)
	if err != nil {
		return 0, false, err
	}
	return reduce(v)
}

func reduce(v model.Value) (float64, bool, error) {
	switch r := v.(type) {
	case model.Vector:
		sum := 0.0
		for _, s := range r {
			// NaN (0/0) would silently compare false against the threshold.
			if math.IsNaN(float64(s.Value)) {
				return 0, false, fmt.Errorf("NaN sample")
			}
			sum += float64(s.Value)
		}
		return sum, len(r) == 0, nil
	case *model.Scalar:
		return float64(r.Value), false, nil
	default:
		return 0, false, fmt.Errorf("unsupported result type %s", v.Type())
	}
}
