package autoscaler

import (
	"context"
	"fmt"
	"log/slog"
	"slices"

	"github.com/Tnze/go-mc/net"
	"github.com/markspolakovs/mcas/metrics"
	"github.com/markspolakovs/mcas/providers/hcloud"
	"github.com/prometheus/common/model"
)

type Autoscaler struct {
	Logger  *slog.Logger
	Metrics *metrics.PrometheusMCMetrics
	Scaler  *hcloud.HCloudAutoscaler

	AllowedScaleProfiles []string

	RconAddress        string
	RconPassword       string
	PreShutdownMessage string
}

func (a *Autoscaler) ConsiderScaleUp(ctx context.Context) (bool, error) {
	r, err := a.Metrics.Query(ctx, `quantile_over_time(0.5, mc_tps[2m])`)
	if err != nil {
		return false, fmt.Errorf("failed to query for median TPS: %w", err)
	}
	v, ok := r.(model.Vector)
	if !ok {
		return false, fmt.Errorf("expected vector result, got %T", r)
	}
	val := v[0].Value
	slog.Debug("considering scale up", slog.Float64("tps", float64(val)))
	return val < 15, nil
}

func (a *Autoscaler) ConsiderScaleDown(ctx context.Context) (bool, error) {
	r, err := a.Metrics.Query(ctx, `sum by (instance) (max_over_time(mc_players_online_total[30m]))`)
	if err != nil {
		return false, fmt.Errorf("failed to query for max players: %w", err)
	}
	v, ok := r.(model.Vector)
	if !ok {
		return false, fmt.Errorf("expected vector result, got %T", r)
	}
	val := v[0].Value
	slog.Debug("considering scale down", slog.Float64("onlinePlayers", float64(val)))
	return val == 0, nil
}

func (a *Autoscaler) PrepareForScalingAction(ctx context.Context) error {
	rcon, err := net.DialRCON(a.RconAddress, a.RconPassword)
	if err != nil {
		return fmt.Errorf("failed to dial RCON: %w", err)
	}
	defer rcon.Close()
	if a.PreShutdownMessage[0] == '{' {
		err = rcon.Cmd(`tellraw @a ` + a.PreShutdownMessage)
	} else {
		err = rcon.Cmd(`say ` + a.PreShutdownMessage)
	}
	if err != nil {
		return fmt.Errorf("failed to send tellraw command: %w", err)
	}
	_, err = rcon.Resp()
	if err != nil {
		return fmt.Errorf("failed to read response from server: %w", err)
	}

	err = rcon.Cmd(`stop`)
	if err != nil {
		return fmt.Errorf("failed to stop server: %w", err)
	}
	_, err = rcon.Resp()
	if err != nil {
		return fmt.Errorf("failed to read response from server: %w", err)
	}
	return nil
}

func (a *Autoscaler) CanScale(ctx context.Context, direction int) (bool, error) {
	profiles, err := a.Scaler.GetAvailableSizes(ctx)
	if err != nil {
		return false, fmt.Errorf("failed to get scale profiles: %w", err)
	}
	profiles = slices.DeleteFunc(profiles, func(s string) bool {
		return !slices.Contains(a.AllowedScaleProfiles, s)
	})
	current, err := a.Scaler.GetCurrentSize(ctx)
	if err != nil {
		return false, fmt.Errorf("failed to get current size: %w", err)
	}
	currentIndex := slices.Index(profiles, current)
	if currentIndex == -1 {
		return false, fmt.Errorf("current size (%s) not found in profiles", current)
	}
	var ok bool
	if direction == 1 {
		ok = currentIndex < len(profiles)-1
	} else if direction == -1 {
		ok = currentIndex > 0
	} else {
		return false, fmt.Errorf("invalid direction %d", direction)
	}
	slog.Debug("can scale", slog.Bool("ok", ok), slog.Int("direction", direction), slog.String("currentProfile", current), slog.Int("currentIndex", currentIndex), slog.Any("profiles", profiles))
	return ok, nil
}

func (a *Autoscaler) DoScale(ctx context.Context, direction int) error {
	profiles, err := a.Scaler.GetAvailableSizes(ctx)
	if err != nil {
		return fmt.Errorf("failed to get scale profiles: %w", err)
	}
	profiles = slices.DeleteFunc(profiles, func(s string) bool {
		return !slices.Contains(a.AllowedScaleProfiles, s)
	})
	current, err := a.Scaler.GetCurrentSize(ctx)
	if err != nil {
		return fmt.Errorf("failed to get current size: %w", err)
	}
	currentIndex := slices.Index(profiles, current)
	if currentIndex == -1 {
		return fmt.Errorf("current size not found in profiles")
	}
	newIndex := currentIndex + direction
	if newIndex < 0 || newIndex >= len(profiles) {
		return fmt.Errorf("invalid new index %d", newIndex)
	}
	newSize := profiles[newIndex]
	slog.Info("scaling", slog.String("current", current), slog.String("new", newSize))
	err = a.PrepareForScalingAction(ctx)
	if err != nil {
		return fmt.Errorf("failed to prepare for scaling action: %w", err)
	}

	slog.Info("stopping server")
	err = a.Scaler.StopServer(ctx)
	if err != nil {
		return fmt.Errorf("failed to stop server: %w", err)
	}

	slog.Info("server stopped, resizing")
	err = a.Scaler.ResizeServer(ctx, newSize)
	if err != nil {
		return fmt.Errorf("failed to resize server: %w", err)
	}

	slog.Info("server resized")
	return nil
}

func (a *Autoscaler) CoreLoop(ctx context.Context) error {
	up, err := a.ConsiderScaleUp(ctx)
	if err != nil {
		return fmt.Errorf("failed to consider scale up: %w", err)
	}
	down, err := a.ConsiderScaleDown(ctx)
	if err != nil {
		return fmt.Errorf("failed to consider scale down: %w", err)
	}
	if down {
		slog.Debug("recommended scale down")
		ok, err := a.CanScale(ctx, -1)
		if err != nil {
			return fmt.Errorf("failed to check if can scale down: %w", err)
		}
		if ok {
			return a.DoScale(ctx, -1)
		} else {
			a.Logger.Info("cannot scale down because there is no eligible profile")
			return nil
		}
	}
	if up {
		slog.Debug("recommended scale up")
		ok, err := a.CanScale(ctx, 1)
		if err != nil {
			return fmt.Errorf("failed to check if can scale up: %w", err)
		}
		if ok {
			return a.DoScale(ctx, 1)
		} else {
			a.Logger.Info("cannot scale up because there is no eligible profile")
			return nil
		}
	}
	a.Logger.Info("no scaling action needed")
	return nil
}
