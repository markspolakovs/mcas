package autoscaler

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Tnze/go-mc/net"
	"github.com/markspolakovs/mcas/metrics"
	"github.com/markspolakovs/mcas/providers/hcloud"
	"github.com/prometheus/common/model"
	"github.com/robfig/cron/v3"
)

type Autoscaler struct {
	Logger  *slog.Logger
	Metrics *metrics.PrometheusMCMetrics
	Scaler  *hcloud.HCloudAutoscaler

	AllowedSizes []string

	RconAddress  string
	RconPassword string

	scalingInProgress atomic.Bool

	Rules    []ScaleRule
	Schedule []ScaleSchedule

	cron *cron.Cron
}

const PreShutdownMessage = `§3Yeti Says: §rServer is eligible for re-sizing. The server will be stopped and resized once nobody is online. The sizing will take a few minutes. If the server is not empty within the next 5 minutes, the re-sizing will be cancelled.`

type ScaleRule struct {
	Query  string `toml:"query"`
	Action int    `toml:"action"`
}

type ScaleSchedule struct {
	Cron   string `toml:"cron"`
	Action int    `toml:"action"`
	IfSize string `toml:"if_size"`

	a   *Autoscaler
	ctx context.Context
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

func (a *Autoscaler) prepareForScalingActionUNLOCKED(ctx context.Context) error {
	rcon, err := net.DialRCON(a.RconAddress, a.RconPassword)
	if err != nil {
		return fmt.Errorf("failed to dial RCON: %w", err)
	}
	defer rcon.Close()
	a.Logger.Debug("sending pre-shutdown message", slog.String("message", PreShutdownMessage))
	if PreShutdownMessage[0] == '{' {
		err = rcon.Cmd(`tellraw @a ` + PreShutdownMessage)
	} else {
		err = rcon.Cmd(`say ` + PreShutdownMessage)
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
	if !a.scalingInProgress.CompareAndSwap(false, true) {
		return fmt.Errorf("scaling already in progress")
	}
	defer a.scalingInProgress.Store(false)
	currentIndex, sizess, err := a.getCurrentSize(ctx)
	if err != nil {
		return fmt.Errorf("failed to get current size: %w", err)
	}

	_, newSize := a.getNewSize(currentIndex, direction, sizess)
	slog.Info("scaling", slog.String("current", sizess[currentIndex]), slog.String("new", newSize))
	err = a.prepareForScalingActionUNLOCKED(ctx)
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

func (a *Autoscaler) SetupSchedule(ctx context.Context) {
	a.cron = cron.New()
	for i := range a.Schedule {
		sch := &a.Schedule[i]
		sch.a = a
		sch.ctx = ctx
		a.cron.AddJob(sch.Cron, sch)
		slog.Debug("loaded schedule", slog.Any("schedule", sch))
	}
	a.cron.Start()
	go func() {
		<-ctx.Done()
		a.cron.Stop()
	}()
}

func (s *ScaleSchedule) Run() {
	slog.Info("considering scheduled scale", slog.Any("schedule", s))
	ctx := s.ctx
	current, sizes, err := s.a.getCurrentSize(ctx)
	if err != nil {
		s.a.Logger.Error("failed to get current size", slog.String("err", err.Error()))
		return
	}
	if s.IfSize != "" {
		if !s.evaluateIfSize(current) {
			s.a.Logger.Info("not scaling because IfSize condition not met")
			return
		}
	}

	ok, err := s.a.CanScale(ctx, s.Action)
	if err != nil {
		s.a.Logger.Error("failed to check if can scale", slog.String("err", err.Error()))
		return
	}
	if !ok {
		s.a.Logger.Debug("not scaling because no eligible size")
		return
	}

	_, newSize := s.a.getNewSize(current, s.Action, sizes)
	s.a.Logger.Info("scheduled scale", slog.String("current", sizes[current]), slog.String("new", newSize))

	err = s.a.DoScale(ctx, s.Action)
	if err != nil {
		s.a.Logger.Error("failed to scale", slog.String("err", err.Error()))
		return
	}
}

func (s *ScaleSchedule) evaluateIfSize(current int) bool {
	op, operandStr, ok := strings.Cut(s.IfSize, " ")
	if !ok {
		s.a.Logger.Error("invalid IfSize", slog.String("IfSize", s.IfSize))
		return false
	}
	operand, err := strconv.Atoi(operandStr)
	if err != nil {
		s.a.Logger.Error("failed to parse operand", slog.String("operand", operandStr), slog.String("err", err.Error()))
		return false
	}
	switch op {
	case ">":
		return current > operand
	case "<":
		return current < operand
	case ">=":
		return current >= operand
	case "<=":
		return current <= operand
	case "==":
	case "=":
		return current == operand
	}
	s.a.Logger.Error("invalid operator", slog.String("operator", op))
	return false
}
