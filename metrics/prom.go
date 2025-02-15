package metrics

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/api"
	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
)

type PrometheusMCMetrics struct {
	address  string
	username string
	password string
	api      v1.API
}

// Create a custom RoundTripper for basic auth
type basicAuthRoundTripper struct {
	username string
	password string
	rt       http.RoundTripper
}

func (b *basicAuthRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req.SetBasicAuth(b.username, b.password)
	return b.rt.RoundTrip(req)
}

func NewPrometheusMCMetrics(address string, username, password string) (*PrometheusMCMetrics, error) {
	cfg := api.Config{
		Address: address,
	}

	if username != "" && password != "" {
		cfg.RoundTripper = &basicAuthRoundTripper{
			username: username,
			password: password,
			rt:       api.DefaultRoundTripper,
		}
	}

	client, err := api.NewClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create prometheus client: %w", err)
	}

	v1api := v1.NewAPI(client)

	return &PrometheusMCMetrics{
		address:  address,
		username: username,
		password: password,
		api:      v1api,
	}, nil
}

func (p *PrometheusMCMetrics) Query(ctx context.Context, query string) (model.Value, error) {
	slog.DebugContext(ctx, "querying prometheus", slog.String("query", query))
	val, _, err := p.api.Query(ctx, query, time.Now())
	if err != nil {
		return nil, fmt.Errorf("failed to query prometheus: %w", err)
	}
	return val, nil
}
