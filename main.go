package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"time"

	"github.com/alecthomas/kong"
	"github.com/markspolakovs/mcas/autoscaler"
	"github.com/markspolakovs/mcas/metrics"
	"github.com/markspolakovs/mcas/providers/hcloud"

	_ "github.com/joho/godotenv/autoload"
)

type Options struct {
	LogLevel slog.Level    `help:"Log level" default:"info" env:"LOG_LEVEL"`
	Interval time.Duration `help:"Interval between checks" default:"1m"`
	Scaler   struct {
		AllowedScaleProfiles []string `help:"List of allowed scale profiles" env:"ALLOWED_SCALE_PROFILES"`
		Hetzner              struct {
			APIKey     string `env:"API_KEY"`
			ServerName string `env:"SERVER_NAME"`
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
		PreShutdownMessage string `help:"Message to send to players before shutting down" env:"PRE_SHUTDOWN_MESSAGE" default:"Server going down for resizing"`
	} `embed:"" prefix:"minecraft."`
}

func main() {
	var args Options
	kongCtx := kong.Parse(&args)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: args.LogLevel,
	}))
	slog.SetDefault(logger)

	metrics, err := metrics.NewPrometheusMCMetrics(args.Metrics.Address, args.Metrics.Username, args.Metrics.Password)
	if err != nil {
		kongCtx.FatalIfErrorf(fmt.Errorf("failed to create prometheus metrics: %w", err))
	}

	scaler, err := hcloud.NewAutoscaler(args.Scaler.Hetzner.APIKey, args.Scaler.Hetzner.ServerName)
	if err != nil {
		kongCtx.FatalIfErrorf(fmt.Errorf("failed to create hcloud autoscaler: %w", err))
	}
	scaler.GetAvailableSizes(context.Background())

	a := &autoscaler.Autoscaler{
		Logger:  logger,
		Metrics: metrics,
		Scaler:  scaler,

		AllowedScaleProfiles: args.Scaler.AllowedScaleProfiles,

		RconAddress:        args.Minecraft.RCON.Address,
		RconPassword:       args.Minecraft.RCON.Password,
		PreShutdownMessage: args.Minecraft.PreShutdownMessage,
	}
	ctx := context.Background()
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt)
	defer cancel()

	logger.Info("core loop starting", slog.Any("interval", args.Interval))
	for {
		logger.Info("core loop iteration")
		err = a.CoreLoop(ctx)
		if err != nil {
			logger.Error("core loop error", slog.String("error", err.Error()))
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(args.Interval):
		}
	}
}
