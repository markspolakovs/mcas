package hcloud

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

type HCloudAutoscaler struct {
	apiKey     string
	serverName string
	api        *hcloud.Client
	server     *hcloud.Server
	opts       HCloudAutoscalerOptions

	serverTypesCache []*hcloud.ServerType
	serverTypesAge   time.Time

	mux sync.Mutex
}

type HCloudAutoscalerOptions struct {
	ServerTypesCacheLifetime time.Duration
}

func NewAutoscaler(apiKey, serverName string, opts HCloudAutoscalerOptions) (*HCloudAutoscaler, error) {
	client := hcloud.NewClient(hcloud.WithToken(apiKey))
	server, _, err := client.Server.GetByName(context.Background(), serverName)
	if err != nil {
		return nil, fmt.Errorf("hcloud: failed to get server by name: %w", err)
	}
	if server == nil {
		return nil, fmt.Errorf("hcloud: server not found")
	}
	return &HCloudAutoscaler{
		apiKey:     apiKey,
		serverName: serverName,
		api:        client,
		server:     server,
		opts:       opts,
	}, nil
}

func (a *HCloudAutoscaler) GetCurrentSize(ctx context.Context) (string, error) {
	a.mux.Lock()
	defer a.mux.Unlock()
	var err error
	a.server, _, err = a.api.Server.GetByID(ctx, a.server.ID)
	if err != nil {
		return "", fmt.Errorf("hcloud: failed to get server by ID: %w", err)
	}
	if a.server == nil {
		return "", fmt.Errorf("hcloud: server not found")
	}
	return a.server.ServerType.Name, nil
}

func (a *HCloudAutoscaler) updateServerTypesUNLOCKED(ctx context.Context) error {
	if a.serverTypesCache != nil && time.Since(a.serverTypesAge) < a.opts.ServerTypesCacheLifetime {
		return nil
	}
	slog.Debug("updating server types cache")
	types, err := a.api.ServerType.All(ctx)
	if err != nil {
		return fmt.Errorf("hcloud: failed to get server types: %w", err)
	}
	a.serverTypesCache = types
	a.serverTypesAge = time.Now()
	return nil
}

func (a *HCloudAutoscaler) GetAvailableSizes(ctx context.Context) ([]string, error) {
	a.mux.Lock()
	defer a.mux.Unlock()
	if a.server != nil {
		var err error
		a.server, _, err = a.api.Server.GetByID(ctx, a.server.ID)
		if err != nil {
			return nil, fmt.Errorf("hcloud: failed to get server by ID: %w", err)
		}
		if a.server == nil {
			return nil, fmt.Errorf("hcloud: server not found")
		}
	}
	err := a.updateServerTypesUNLOCKED(ctx)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(a.serverTypesCache, func(a, b *hcloud.ServerType) int {
		c1, err := strconv.ParseFloat(a.Pricings[0].Hourly.Gross, 64)
		if err != nil {
			panic(err)
		}
		c2, err := strconv.ParseFloat(b.Pricings[0].Hourly.Gross, 64)
		if err != nil {
			panic(err)
		}
		if c1 < c2 {
			return -1
		}
		if c1 > c2 {
			return 1
		}
		return 0
	})
	rv := make([]string, 0, len(a.serverTypesCache))
	for _, t := range a.serverTypesCache {
		if t.Architecture == a.server.ServerType.Architecture {
			for _, pricing := range t.Pricings {
				if pricing.Location.Name == a.server.Datacenter.Location.Name {
					rv = append(rv, t.Name)
					break
				}
			}
		}
	}
	return rv, nil
}

func (a *HCloudAutoscaler) StopServer(ctx context.Context) error {
	a.mux.Lock()
	defer a.mux.Unlock()
	action, _, err := a.api.Server.Shutdown(ctx, a.server)
	if err != nil {
		return fmt.Errorf("hcloud: failed to shutdown server: %w", err)
	}
	if action.Status == hcloud.ActionStatusSuccess {
		return nil
	}
	err = a.waitForAction(ctx, action)
	if err != nil {
		return fmt.Errorf("hcloud: failed to shutdown server: %w", err)
	}
	slog.Debug("server stopped, waiting for it to actually stop")
	// stopped doesn't actually mean stopped, sadge. poll until it's really stopped.
	for {
		a.server, _, err = a.api.Server.GetByID(ctx, a.server.ID)
		if err != nil {
			return fmt.Errorf("hcloud: failed to get server by ID: %w", err)
		}
		if a.server.Status == hcloud.ServerStatusOff {
			break
		}
		slog.Debug("... still waiting ...", slog.Any("status", a.server.Status))
		select {
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (a *HCloudAutoscaler) ResizeServer(ctx context.Context, profile string) error {
	a.mux.Lock()
	defer a.mux.Unlock()
	err := a.updateServerTypesUNLOCKED(ctx)
	if err != nil {
		return err
	}
	var serverType *hcloud.ServerType
	for _, t := range a.serverTypesCache {
		if t.Name == profile {
			serverType = t
			break
		}
	}

	if serverType == nil {
		return fmt.Errorf("hcloud: server type not found: %s", profile)
	}

	err = a.resizeServerInner(ctx, serverType)
	if err != nil {
		slog.Warn("hcloud: server resize failed, starting up manually", slog.String("err", err.Error()))
		// Start it up again
		_, _, err := a.api.Server.Poweron(ctx, a.server)
		if err != nil {
			return fmt.Errorf("hcloud: failed to power on server: %w", err)
		}
	}
	return err
}

func (a *HCloudAutoscaler) resizeServerInner(ctx context.Context, serverType *hcloud.ServerType) error {
	action, _, err := a.api.Server.ChangeType(ctx, a.server, hcloud.ServerChangeTypeOpts{
		ServerType:  serverType,
		UpgradeDisk: false,
	})
	if err != nil {
		return fmt.Errorf("hcloud: failed to resize server: %w", err)
	}
	if action.Status == hcloud.ActionStatusSuccess {
		return nil
	}
	return a.waitForAction(ctx, action)
}

func (a *HCloudAutoscaler) waitForAction(ctx context.Context, action *hcloud.Action) error {
	attempt := 0
	for {
		if attempt > 24 {
			return fmt.Errorf("hcloud: action %d did not complete in time", action.ID)
		}
		attempt++
		action, _, err := a.api.Action.GetByID(ctx, action.ID)
		if err != nil {
			return fmt.Errorf("hcloud: failed to get action: %w", err)
		}
		slog.Debug("action status", slog.Int64("id", action.ID), slog.String("status", string(action.Status)))
		if action.Status == hcloud.ActionStatusSuccess {
			return nil
		}
		if action.Status == hcloud.ActionStatusError {
			return fmt.Errorf("hcloud: action failed: %w", action.Error())
		}
		select {
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
