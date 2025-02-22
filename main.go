package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/alecthomas/kong"

	"github.com/markspolakovs/mcas/autoscaler"
	"github.com/markspolakovs/mcas/metrics"
	"github.com/markspolakovs/mcas/providers/hcloud"

	_ "github.com/joho/godotenv/autoload"
)

type Options struct {
	LogLevel            slog.Level    `help:"Log level" default:"info" env:"LOG_LEVEL"`
	Interval            time.Duration `help:"Interval between checks" default:"1m" env:"INTERVAL"`
	MinTimeBetweenScale time.Duration `help:"Minimum time between scaling" default:"1h" env:"MIN_TIME_BETWEEN_SCALE"`
	RulesFile           string        `help:"Path to the rules file" env:"RULES_FILE"`
	Scaler              struct {
		AllowedServerSizes []string `help:"List of allowed server sizes" env:"ALLOWED_SIZES"`
		Hetzner            struct {
			APIKey               string        `env:"API_KEY"`
			ServerName           string        `env:"SERVER_NAME"`
			ServerTypesCacheTime time.Duration `help:"Server types cache time" default:"10m" env:"SERVER_TYPES_CACHE_TIME"`
		} `embed:"" envprefix:"HETZNER_" prefix:"hetzner."`
	} `embed:"" prefix:"scaler."`
	Metrics struct {
		Address  string `help:"Prometheus address" env:"ADDRESS"`
		Username string `help:"Prometheus username" env:"USERNAME"`
		Password string `help:"Prometheus password" env:"PASSWORD"`
	} `embed:"" prefix:"metrics." envprefix:"METRICS_"`
	Minecraft struct {
		RCON struct {
			Address  string `help:"RCON address" env:"ADDRESS"`
			Password string `help:"RCON password" env:"PASSWORD"`
		} `embed:"" prefix:"rcon." envprefix:"RCON_"`
	} `embed:"" prefix:"minecraft."`
}

func loadRules(args Options) ([]autoscaler.ScaleRule, []autoscaler.ScaleSchedule, error) {
	var data struct {
		Rules    []autoscaler.ScaleRule     `toml:"rules"`
		Schedule []autoscaler.ScaleSchedule `toml:"schedule"`
	}
	_, err := toml.DecodeFile(args.RulesFile, &data)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to load rules file: %w", err)
	}
	return data.Rules, data.Schedule, nil
}

func main() {
	var args Options
	kongCtx := kong.Parse(&args)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: args.LogLevel,
	}))
	slog.SetDefault(logger)

	rules, schedule, err := loadRules(args)
	if err != nil {
		kongCtx.FatalIfErrorf(err)
	}
	logger.Debug("loaded rules", slog.Any("rules", rules))

	metrics, err := metrics.NewPrometheusMCMetrics(args.Metrics.Address, args.Metrics.Username, args.Metrics.Password)
	if err != nil {
		kongCtx.FatalIfErrorf(fmt.Errorf("failed to create prometheus metrics: %w", err))
	}

	scaler, err := hcloud.NewAutoscaler(args.Scaler.Hetzner.APIKey, args.Scaler.Hetzner.ServerName, hcloud.HCloudAutoscalerOptions{
		ServerTypesCacheLifetime: args.Scaler.Hetzner.ServerTypesCacheTime,
	})
	if err != nil {
		kongCtx.FatalIfErrorf(fmt.Errorf("failed to create hcloud autoscaler: %w", err))
	}

	a := autoscaler.NewAutoscaler(autoscaler.AutoScalerConfig{
		Logger:  logger,
		Metrics: metrics,
		Scaler:  scaler,

		AllowedSizes: args.Scaler.AllowedServerSizes,
		Rules:        rules,
		Schedule:     schedule,

		RconAddress:  args.Minecraft.RCON.Address,
		RconPassword: args.Minecraft.RCON.Password,
	})

	ctx := context.Background()
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt)
	defer cancel()

	a.SetupSchedule(ctx)

	logger.Info("core loop starting", slog.Any("interval", args.Interval))
	for {
		logger.Info("core loop iteration")
		err = a.CoreLoop(ctx)
		if err != nil {
			logger.Error("core loop error", slog.String("error", err.Error()))
		}
		select {
		case last := <-a.LastScaledAt():
			next := last.Add(args.MinTimeBetweenScale)
			logger.Info("waiting before scaling again", slog.Time("next", next))
			select {
			case <-time.After(time.Until(next)):
			case <-ctx.Done():
				return
			}
		default:
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(args.Interval):
		}
	}
}
