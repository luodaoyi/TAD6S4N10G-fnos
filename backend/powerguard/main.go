package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"fnos-powerguard/internal/powerguard"
)

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(os.Args[2:])
	case "probe":
		err = probe(os.Args[2:])
	case "init":
		err = initialize(os.Args[2:])
	case "apply":
		err = apply(os.Args[2:])
	case "restore":
		err = restore(os.Args[2:])
	case "version":
		fmt.Println(version)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "tad-module:", err)
		os.Exit(1)
	}
}

func commonFlags(name string, args []string) (*powerguard.Manager, *flag.FlagSet, error) {
	set := flag.NewFlagSet(name, flag.ContinueOnError)
	config := set.String("config", "", "config file")
	state := set.String("state", "", "state file")
	root := set.String("sys-root", "/", "alternate system root (testing only)")
	if err := set.Parse(args); err != nil {
		return nil, nil, err
	}
	if *config == "" || *state == "" {
		return nil, nil, errors.New("--config and --state are required")
	}
	return &powerguard.Manager{Root: *root, ConfigPath: *config, StatePath: *state, Version: version}, set, nil
}

func initialize(args []string) error {
	manager, _, err := commonFlags("init", args)
	if err != nil {
		return err
	}
	if err := requireRoot(); err != nil {
		return err
	}
	if _, err := manager.LoadOrCreateConfig(); err != nil {
		return err
	}
	return manager.ApplyCurrent()
}

func apply(args []string) error {
	manager, _, err := commonFlags("apply", args)
	if err != nil {
		return err
	}
	if err := requireRoot(); err != nil {
		return err
	}
	return manager.ApplyCurrent()
}

func restore(args []string) error {
	manager, _, err := commonFlags("restore", args)
	if err != nil {
		return err
	}
	if err := requireRoot(); err != nil {
		return err
	}
	err = manager.Restore()
	if errors.Is(err, powerguard.ErrOriginalStateCPUMismatch) {
		fmt.Fprintln(os.Stderr, "tad-module: skipped restoring power and fan state captured on different hardware")
		return nil
	}
	return err
}

func probe(args []string) error {
	set := flag.NewFlagSet("probe", flag.ContinueOnError)
	root := set.String("sys-root", "/", "alternate system root")
	asJSON := set.Bool("json", false, "JSON output")
	if err := set.Parse(args); err != nil {
		return err
	}
	manager := &powerguard.Manager{Root: *root, Version: version}
	model, err := manager.CPUModel()
	if err != nil {
		return err
	}
	profile, err := powerguard.DetectProfile(model)
	if err != nil {
		return err
	}
	packages, err := manager.DiscoverPackages()
	if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"cpu_model": model, "profile": profile, "packages": packages})
	}
	fmt.Printf("CPU: %s\nProfile: %s\nRAPL packages: %d\n", model, profile.ID, len(packages))
	return nil
}

func serve(args []string) error {
	set := flag.NewFlagSet("serve", flag.ContinueOnError)
	config := set.String("config", "", "config file")
	state := set.String("state", "", "state file")
	socket := set.String("socket", "", "Unix socket")
	webRoot := set.String("web-root", "", "static web root")
	logPath := set.String("log", "", "log file")
	root := set.String("sys-root", "/", "alternate system root (testing only)")
	if err := set.Parse(args); err != nil {
		return err
	}
	if *config == "" || *state == "" || *socket == "" || *webRoot == "" {
		return errors.New("--config, --state, --socket and --web-root are required")
	}
	if err := requireRoot(); err != nil {
		return err
	}
	logger := log.Default()
	if *logPath != "" {
		if err := os.MkdirAll(filepath.Dir(*logPath), 0o700); err != nil {
			return err
		}
		file, err := os.OpenFile(*logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer file.Close()
		logger = log.New(file, "", log.LstdFlags|log.LUTC)
	}
	manager := &powerguard.Manager{Root: *root, ConfigPath: *config, StatePath: *state, Version: version}
	cfg, err := manager.LoadOrCreateConfig()
	if err != nil {
		return err
	}
	if err := manager.ApplyCurrent(); err != nil {
		logger.Printf("initial apply failed; continuing in degraded mode so configuration can be repaired: %v", err)
	}
	logger.Printf("started version=%s interval=%ds", version, cfg.ReapplySeconds)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		server := &powerguard.Server{Manager: manager, Socket: *socket, WebRoot: *webRoot, BasePath: "/app/tad-module", Logger: logger}
		done <- server.ListenAndServe()
	}()
	go reapplyLoop(ctx, manager, logger)
	go fanLoop(ctx, manager, logger)
	go storageLoop(ctx, manager, logger)
	go storageActivityLoop(ctx, manager)
	go gpioLoop(ctx, manager, logger)

	select {
	case <-ctx.Done():
		logger.Printf("stopping: restoring captured power and fan settings")
		err := manager.Restore()
		if errors.Is(err, powerguard.ErrOriginalStateCPUMismatch) {
			logger.Printf("restore skipped because saved state belongs to different hardware: %v", err)
			return nil
		}
		return err
	case err := <-done:
		logger.Printf("server stopped: restoring captured power and fan settings")
		restoreErr := manager.Restore()
		if errors.Is(restoreErr, powerguard.ErrOriginalStateCPUMismatch) {
			logger.Printf("restore skipped because saved state belongs to different hardware: %v", restoreErr)
			restoreErr = nil
		}
		return errors.Join(err, restoreErr)
	}
}

func reapplyLoop(ctx context.Context, manager *powerguard.Manager, logger *log.Logger) {
	for {
		cfg, err := manager.LoadOrCreateConfig()
		interval := 30 * time.Second
		if err == nil && cfg.ReapplySeconds >= 5 && cfg.ReapplySeconds <= 300 {
			interval = time.Duration(cfg.ReapplySeconds) * time.Second
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			if err := manager.ApplyCurrent(); err != nil {
				logger.Printf("reapply failed: %v", err)
			}
		}
	}
}

func fanLoop(ctx context.Context, manager *powerguard.Manager, logger *log.Logger) {
	for {
		cfg, err := manager.LoadOrCreateConfig()
		interval := 2 * time.Second
		if err == nil && cfg.Fan.PollSeconds >= 1 && cfg.Fan.PollSeconds <= 10 {
			interval = time.Duration(cfg.Fan.PollSeconds) * time.Second
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			if err := manager.ApplyFanCurrent(); err != nil {
				logger.Printf("fan control failed: %v", err)
			}
		}
	}
}

func storageActivityLoop(ctx context.Context, manager *powerguard.Manager) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			manager.RefreshStorageActivity()
		}
	}
}

func storageLoop(ctx context.Context, manager *powerguard.Manager, logger *log.Logger) {
	refresh := func() {
		status := manager.RefreshStorage(false)
		if status.LastError != "" {
			logger.Printf("storage refresh partially failed: %s", status.LastError)
		}
	}
	refresh()
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refresh()
		}
	}
}

func gpioLoop(ctx context.Context, manager *powerguard.Manager, logger *log.Logger) {
	actions := make(chan powerguard.GPIOEvent, 4)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case event := <-actions:
				output, err := manager.ExecuteGPIOActionContext(ctx, event)
				if output != "" {
					logger.Printf("gpio script output button=%s action=%s: %s", event.ButtonID, event.Action, output)
				}
				if err != nil {
					logger.Printf("gpio action failed button=%s action=%s: %v", event.ButtonID, event.Action, err)
				}
			}
		}
	}()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var port *os.File
	defer func() {
		if port != nil {
			_ = port.Close()
		}
	}()
	var config powerguard.GPIOConfig
	var nextConfigLoad time.Time
	var lastLoggedError string
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if !now.Before(nextConfigLoad) {
				cfg, err := manager.LoadOrCreateConfig()
				if err != nil {
					if err.Error() != lastLoggedError {
						logger.Printf("gpio config failed: %v", err)
						lastLoggedError = err.Error()
					}
					nextConfigLoad = now.Add(time.Second)
					continue
				}
				config = cfg.GPIO
				nextConfigLoad = now.Add(time.Second)
			}
			if !config.Enabled {
				if port != nil {
					_ = port.Close()
					port = nil
				}
				manager.ResetGPIO(false, false, nil)
				lastLoggedError = ""
				continue
			}
			if port == nil {
				var err error
				port, err = os.Open(manager.GPIOPortPath())
				if err != nil {
					manager.ResetGPIO(true, false, err)
					if err.Error() != lastLoggedError {
						logger.Printf("gpio open failed: %v", err)
						lastLoggedError = err.Error()
					}
					continue
				}
			}
			events, err := manager.PollGPIO(config, port, now)
			if err != nil {
				if err.Error() != lastLoggedError {
					logger.Printf("gpio poll failed: %v", err)
					lastLoggedError = err.Error()
				}
				_ = port.Close()
				port = nil
				continue
			}
			lastLoggedError = ""
			for _, event := range events {
				if event.IsFeedback() {
					logger.Printf("gpio hold confirmed button=%s stage=%s", event.ButtonID, event.Stage)
					go func(feedback powerguard.GPIOEvent) {
						if err := manager.PlayGPIOFeedback(feedback); err != nil {
							logger.Printf("gpio feedback unavailable button=%s stage=%s: %v", feedback.ButtonID, feedback.Stage, err)
						}
					}(event)
					continue
				}
				logger.Printf("gpio event button=%s stage=%s duration=%s action=%s", event.ButtonID, event.Stage, event.Duration.Round(100*time.Millisecond), event.Action)
				select {
				case actions <- event:
				default:
					err := errors.New("gpio action queue is full")
					manager.SetGPIOActionError(err)
					logger.Printf("gpio action dropped button=%s action=%s: %v", event.ButtonID, event.Action, err)
				}
			}
		}
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: tad-module <serve|probe|init|apply|restore|version> [options]")
}
