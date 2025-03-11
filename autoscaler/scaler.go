package autoscaler

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/Tnze/go-mc/net"
	"github.com/markspolakovs/mcas/metrics"
	"github.com/markspolakovs/mcas/providers/hcloud"
	"github.com/robfig/cron/v3"
)

type AutoScalerConfig struct {
	Logger  *slog.Logger
	Metrics *metrics.PrometheusMCMetrics
	Scaler  *hcloud.HCloudAutoscaler

	AllowedSizes []string

	RconAddress  string
	RconPassword string

	MinTimeBetweenActions time.Duration

	PreShutdownMessage string

	Rules    []ScaleRule
	Schedule []ScaleSchedule
}

// Ensures that an Autoscaler cannot be created except by using NewAutoscaler
type cfg = AutoScalerConfig
type Autoscaler struct {
	cfg

	scaleLock    sync.Mutex
	cron         *cron.Cron
	lastScaledAt time.Time
}

func NewAutoscaler(cfg AutoScalerConfig) *Autoscaler {
	return &Autoscaler{
		cfg: cfg,
	}
}

func (a *Autoscaler) getCurrentSize(ctx context.Context) (int, []string, error) {
	sizes, err := a.Scaler.GetAvailableSizes(ctx)
	if err != nil {
		return 0, nil, fmt.Errorf("failed to get scale sizes: %w", err)
	}
	slog.Debug("available sizes", slog.Any("sizes", sizes))
	sizes = slices.DeleteFunc(sizes, func(s string) bool {
		return !slices.Contains(a.AllowedSizes, s)
	})
	slog.Debug("allowed sizes", slog.Any("sizes", sizes))
	current, err := a.Scaler.GetCurrentSize(ctx)
	if err != nil {
		return 0, nil, fmt.Errorf("failed to get current size: %w", err)
	}
	currentIndex := slices.Index(sizes, current)
	if currentIndex == -1 {
		return 0, nil, fmt.Errorf("current size (%s) not found in sizes", current)
	}
	return currentIndex, sizes, nil
}

func (a *Autoscaler) getNewSize(current, action int, sizes []string) (int, string) {
	newIndex := current + action
	if newIndex < 0 {
		newIndex = 0
	}
	if newIndex >= len(sizes) {
		newIndex = len(sizes) - 1
	}
	return newIndex, sizes[newIndex]
}

func (a *Autoscaler) CanScale(ctx context.Context, direction int) (bool, error) {
	currentIndex, sizes, err := a.getCurrentSize(ctx)
	if err != nil {
		return false, fmt.Errorf("failed to get current size: %w", err)
	}
	newIndex, newSize := a.getNewSize(currentIndex, direction, sizes)
	ok := newIndex != currentIndex
	slog.Debug("can scale", slog.Bool("ok", ok), slog.Int("direction", direction), slog.String("currentSize", sizes[currentIndex]), slog.Int("currentIndex", currentIndex), slog.Any("sizes", sizes))
	if !ok {
		a.Logger.Info("cannot scale because there is no eligible size", slog.String("current", sizes[currentIndex]), slog.String("new", newSize), slog.Int("direction", direction), slog.Any("sizes", sizes))
	}
	return ok, nil
}

func (a *Autoscaler) prepareForScalingAction(ctx context.Context) error {
	rcon, err := net.DialRCON(a.RconAddress, a.RconPassword)
	if err != nil {
		return fmt.Errorf("failed to dial RCON: %w", err)
	}
	defer rcon.Close()
	a.Logger.Debug("sending pre-shutdown message", slog.String("message", a.PreShutdownMessage))
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

	err = waitForServerToBeEmpty(ctx, rcon, 5*time.Minute)
	if err != nil {
		return fmt.Errorf("failed to wait for server to be empty: %w", err)
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

var listRe = regexp.MustCompile(`There are (\d+) out of maximum \d+ players online\..*`)
var formatRe = regexp.MustCompile(`§[0-9a-z]`)

func waitForServerToBeEmpty(ctx context.Context, rcon net.RCONClientConn, timeout time.Duration) error {
	deadline := time.After(timeout)
	for {
		err := rcon.Cmd(`list`)
		if err != nil {
			return fmt.Errorf("failed to send list command: %w", err)
		}
		resp, err := rcon.Resp()
		if err != nil {
			return fmt.Errorf("failed to read response: %w", err)
		}
		slog.Debug("list response", slog.String("response", resp))
		resp = formatRe.ReplaceAllString(resp, "")
		match := listRe.FindStringSubmatch(resp)
		if match == nil {
			return fmt.Errorf("list response does not match expected format: %q", resp)
		}
		slog.Info("online players", slog.String("count", match[1]))
		if match[1] == "0" {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("server not empty after %s", timeout)
		case <-time.After(5 * time.Second):
		}
	}
}

func (a *Autoscaler) DoScale(ctx context.Context, direction int) error {
	if !a.scaleLock.TryLock() {
		return fmt.Errorf("scaling already in progress")
	}
	defer a.scaleLock.Unlock()
	if a.lastScaledAt.Add(a.MinTimeBetweenActions).After(time.Now()) {
		return fmt.Errorf("scaling too soon")
	}
	currentIndex, sizess, err := a.getCurrentSize(ctx)
	if err != nil {
		return fmt.Errorf("failed to get current size: %w", err)
	}

	_, newSize := a.getNewSize(currentIndex, direction, sizess)
	slog.Info("scaling", slog.String("current", sizess[currentIndex]), slog.String("new", newSize))
	err = a.prepareForScalingAction(ctx)
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
	a.lastScaledAt = time.Now()
	return nil
}
