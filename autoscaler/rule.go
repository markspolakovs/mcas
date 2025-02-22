package autoscaler

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/prometheus/common/model"
)

type ScaleRule struct {
	Query  string `toml:"query"`
	Action int    `toml:"action"`
}

func (a *Autoscaler) EvaluateRule(ctx context.Context, rule ScaleRule) (bool, error) {
	r, err := a.Metrics.Query(ctx, rule.Query)
	if err != nil {
		return false, fmt.Errorf("failed to query for rule %q: %w", rule.Query, err)
	}
	v, ok := r.(model.Vector)
	if !ok {
		return false, fmt.Errorf("expected vector result, got %T", r)
	}
	slog.Debug("evaluating rule", slog.String("query", rule.Query), slog.Any("result", v))
	return len(v) > 0, nil
}

func (a *Autoscaler) CoreLoop(ctx context.Context) error {
	if a.scalingInProgress.Load() {
		a.Logger.Info("scaling in progress, skipping")
		return nil
	}
	for _, rule := range a.Rules {
		res, err := a.EvaluateRule(ctx, rule)
		if err != nil {
			return fmt.Errorf("failed to evaluate rule: %w", err)
		}
		if !res {
			slog.Debug("rule not met", slog.String("query", rule.Query))
			continue
		}
		slog.Info("rule met", slog.String("query", rule.Query), slog.Int("action", rule.Action))
		ok, err := a.CanScale(ctx, rule.Action)
		if err != nil {
			return fmt.Errorf("failed to check if can scale: %w", err)
		}
		if ok {
			return a.DoScale(ctx, rule.Action)
		} else {
			return nil
		}
	}
	a.Logger.Info("no scaling action needed")
	return nil
}
