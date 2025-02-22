package autoscaler

import (
	"context"
	"log/slog"
	"strconv"
	"strings"

	"github.com/robfig/cron/v3"
)

type ScaleSchedule struct {
	Cron   string `toml:"cron"`
	Action int    `toml:"action"`
	IfSize string `toml:"if_size"`

	a   *Autoscaler
	ctx context.Context
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
