package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/machine"
	"github.com/cedar2025/xboard-node/internal/nlog"
	"github.com/cedar2025/xboard-node/internal/panel"
	"github.com/cedar2025/xboard-node/internal/service"
)

var (
	version   = "dev"
	buildTime = "unknown"
)

func main() {
	configPath := flag.String("c", "config.yml", "config file path")
	showVersion := flag.Bool("v", false, "show version")
	flag.Parse()

	if *showVersion {
		fmt.Printf("xboard-node %s (built %s)\n", version, buildTime)
		os.Exit(0)
	}

	rootCfg, err := config.LoadRoot(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}

	instances, err := rootCfg.NormalizeInstances()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to normalize config: %v\n", err)
		os.Exit(1)
	}
	config.InitLogger(instances[0].Log)

	// Apply runtime memory tuning before anything else allocates.
	applyRuntimeConfig(instances[0].Runtime)

	runWithReload(rootCfg, *configPath)
}

// runWithReload restarts all node services when the config file changes.
func runWithReload(initialRoot *config.RootConfig, configPath string) {
	var healthSrv *http.Server
	var healthPort int
	// startHealth (re)binds the /healthz listener. A bind failure no
	// longer kills the process: the old listener (if any) keeps
	// serving, and a first-start failure just leaves the node without
	// a health endpoint — traffic still flows, and the error log plus
	// the installer's health check surface the problem. Previously a
	// transient port conflict during reload exited the whole node.
	startHealth := func(port int) {
		if port <= 0 {
			if healthSrv != nil {
				healthSrv.Close()
				healthSrv = nil
			}
			healthPort = 0
			return
		}
		// Bind the new listener BEFORE closing the old one, so a
		// failure here keeps the previous listener alive.
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
		if err != nil {
			nlog.Core().Error("failed to start health check listener, keeping previous listener", "port", port, "error", err)
			return
		}
		if healthSrv != nil {
			healthSrv.Close()
			healthSrv = nil
		}
		mux := http.NewServeMux()
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"status":"ok"}`))
		})
		healthSrv = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		healthPort = port
		go func() {
			nlog.Core().Debug(fmt.Sprintf("health check listening on :%d", port))
			if err := healthSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
				nlog.Core().Warn("health check server stopped", "error", err)
			}
		}()
	}

	initialInstances, err := initialRoot.NormalizeInstances()
	if err != nil {
		nlog.Core().Error("failed to normalize initial config", "error", err)
		os.Exit(1)
	}
	startHealth(initialInstances[0].HealthPort)
	defer func() {
		if healthSrv != nil {
			healthSrv.Close()
		}
	}()

	for root := initialRoot; ; {
		ctx, cancel := context.WithCancel(context.Background())

		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

		reloadCh := make(chan *config.RootConfig, 1)

		watcher, err := config.WatchConfigRoot(ctx, configPath, func(newCfg *config.RootConfig) {
			select {
			case reloadCh <- newCfg:
			default:
			}
		})
		if err != nil {
			nlog.Core().Warn("config watcher unavailable, hot-reload disabled", "error", err)
		}

		go func() {
			select {
			case sig := <-sigCh:
				nlog.Core().Info(fmt.Sprintf("received %v, shutting down...", sig))
				cancel()

				select {
				case sig = <-sigCh:
					nlog.Core().Warn("received second signal, forcing exit", "signal", sig)
					os.Exit(1)
				case <-time.After(15 * time.Second):
					nlog.Core().Error("shutdown timed out after 15s, forcing exit")
					os.Exit(2)
				}
			case <-ctx.Done():
			}
		}()

		instances, err := root.NormalizeInstances()
		if err != nil {
			nlog.Core().Error("failed to normalize config", "error", err)
			os.Exit(1)
		}
		if err := config.ValidateStartupLayout(instances); err != nil {
			nlog.Core().Error("startup layout validation failed", "error", err)
			os.Exit(1)
		}

		warnFirstInstanceOnly(instances)
		if instances[0].HealthPort != healthPort {
			// startHealth binds first and only then closes the old
			// listener, so a failed rebind keeps the old endpoint.
			startHealth(instances[0].HealthPort)
		}

		// Buffer for one error per running service, not per instance:
		// a single instance in multi-node mode runs len(nodes)
		// services, and each failing service sends one error. Sizing
		// by instance count deadlocked single-instance multi-node
		// setups: the second failing node blocked on the send, its
		// WaitGroup never finished, and the process never exited.
		// Sends below are additionally non-blocking (first error
		// wins; every error is already logged), so a future sender
		// can never block shutdown even if the count is off.
		totalServices := countServices(instances)
		errCh := make(chan error, totalServices)
		sendErr := func(err error) {
			select {
			case errCh <- err:
			default:
			}
		}
		doneCh := make(chan struct{})
		var wg sync.WaitGroup
		for _, instanceCfg := range instances {
			instanceCfg := instanceCfg
			wg.Add(1)
			go func() {
				defer wg.Done()
				if instanceCfg.IsMachineMode() {
					// B7: Panel.URL is normally token-free (the token is a
					// separate config field), but redact it anyway so a
					// token smuggled into the URL field can never leak here.
					nlog.Core().Info("starting machine instance", "instance", instanceCfg.InstanceID, "machine_id", instanceCfg.Machine.MachineID, "panel_url", panel.RedactURL(instanceCfg.Panel.URL))
					orch := machine.New(instanceCfg)
					if err := orch.Run(ctx); err != nil {
						nlog.Core().Error("machine instance exited with error", "instance", instanceCfg.InstanceID, "error", err)
						sendErr(err)
						cancel()
					}
					return
				}
				nodes := instanceCfg.ExpandNodes()
				// B7: see the machine-instance log line above.
				nlog.Core().Info("starting node instance", "instance", instanceCfg.InstanceID, "nodes", len(nodes), "panel_url", panel.RedactURL(instanceCfg.Panel.URL))
				var instanceWG sync.WaitGroup
				for idx, nodeCfg := range nodes {
					nodeCfg := nodeCfg
					instanceWG.Add(1)
					go func(idx int) {
						defer instanceWG.Done()
						if idx > 0 {
							delay := time.Duration(idx) * 250 * time.Millisecond
							if delay > 2*time.Second {
								delay = 2 * time.Second
							}
							select {
							case <-time.After(delay):
							case <-ctx.Done():
								return
							}
						}
						svc := service.New(nodeCfg)
						if err := svc.Run(ctx); err != nil {
							nlog.Core().Error("node service exited with error", "instance", nodeCfg.InstanceID, "node_id", nodeCfg.Panel.NodeID, "error", err)
							sendErr(err)
							cancel()
						}
					}(idx)
				}
				instanceWG.Wait()
			}()
		}
		go func() { wg.Wait(); close(doneCh) }()

		var newRoot *config.RootConfig
		select {
		case newRoot = <-reloadCh:
			nlog.Core().Info("config changed, restarting all services...")
			cancel()
			// Bounded wait, same 15s ceiling as the signal path:
			// an unbounded wait here could hang the reload forever
			// if a service ignores cancellation, and a signal
			// arriving mid-reload would otherwise force-exit the
			// process while the replacement services are starting.
			// On timeout the restart still proceeds; the old
			// services' contexts are already cancelled, so they
			// finish exiting on their own.
			select {
			case <-doneCh:
			case <-time.After(15 * time.Second):
				nlog.Core().Error("reload shutdown timed out after 15s, proceeding with restart; old services may still be stopping")
			}
		case <-doneCh:
		}

		signal.Stop(sigCh)
		if watcher != nil {
			watcher.Stop()
		}

		if newRoot == nil {
			close(errCh)
			if err := <-errCh; err != nil {
				os.Exit(1)
			}
			nlog.Core().Info("stopped")
			return
		}

		newInstances, err := newRoot.NormalizeInstances()
		if err != nil {
			nlog.Core().Error("failed to normalize reloaded config", "error", err)
			os.Exit(1)
		}
		config.InitLogger(newInstances[0].Log)
		applyRuntimeConfig(newInstances[0].Runtime)
		root = newRoot
		nlog.Core().Info("reload complete, services restarting with new config")
	}
}

// countServices counts the services that will actually run: one per
// machine-mode instance, one per node entry otherwise. It sizes the
// error channel in runWithReload (see the comment there).
func countServices(instances []*config.Config) int {
	total := 0
	for _, inst := range instances {
		if inst.IsMachineMode() {
			total++
		} else {
			total += len(inst.ExpandNodes())
		}
	}
	if total < 1 {
		total = 1
	}
	return total
}

// warnFirstInstanceOnly logs an explicit warning for every later
// instance whose health-port, log or runtime settings differ from the
// first instance's. Only the first instance's values are ever used
// (InitLogger, applyRuntimeConfig and startHealth all read
// instances[0]); previously the others were silently ignored, so an
// operator could believe a per-instance health port or log level was
// in effect when it was not.
func warnFirstInstanceOnly(instances []*config.Config) {
	if len(instances) <= 1 {
		return
	}
	first := instances[0]
	for _, inst := range instances[1:] {
		if inst.HealthPort != first.HealthPort {
			nlog.Core().Warn("multi-instance: only the first instance's health_port takes effect; this instance's health_port is ignored",
				"instance", inst.InstanceID, "health_port", inst.HealthPort, "effective_health_port", first.HealthPort)
		}
		if inst.Log != first.Log {
			nlog.Core().Warn("multi-instance: only the first instance's log settings take effect; this instance's log settings are ignored",
				"instance", inst.InstanceID, "log", inst.Log, "effective_log", first.Log)
		}
		if inst.Runtime != first.Runtime {
			nlog.Core().Warn("multi-instance: only the first instance's runtime settings take effect; this instance's runtime settings are ignored",
				"instance", inst.InstanceID, "runtime", inst.Runtime, "effective_runtime", first.Runtime)
		}
	}
}

// applyRuntimeConfig wires up Go runtime memory limits from the config file.
// Both settings can also be overridden by environment variables (GOMEMLIMIT /
// GOGC) — the env vars take precedence because Go's runtime reads them before
// we can call these functions, but we set them here for completeness and so
// the values are logged.
func applyRuntimeConfig(rt config.RuntimeConfig) {
	// GOGC
	if rt.GoGCPercent > 0 {
		prev := debug.SetGCPercent(rt.GoGCPercent)
		nlog.Core().Info("runtime: GOGC set", "gogc", rt.GoGCPercent, "prev", prev)
	}

	// GOMEMLIMIT — parse human-readable size string (e.g. "30MiB")
	if rt.GoMemLimit != "" {
		limit, err := parseMemLimit(rt.GoMemLimit)
		if err != nil {
			nlog.Core().Warn("runtime: invalid gomemlimit, ignoring", "value", rt.GoMemLimit, "error", err)
		} else {
			prev := debug.SetMemoryLimit(limit)
			nlog.Core().Info("runtime: GOMEMLIMIT set",
				"limit", rt.GoMemLimit,
				"bytes", limit,
				"prev_bytes", prev,
			)
		}
	}
}

// parseMemLimit converts a human-readable size string to bytes.
// Supported suffixes: B, KiB, MiB, GiB, TiB (case-insensitive).
func parseMemLimit(s string) (int64, error) {
	s = strings.TrimSpace(s)
	suffixes := []struct {
		suffix string
		mult   int64
	}{
		{"TiB", 1 << 40},
		{"GiB", 1 << 30},
		{"MiB", 1 << 20},
		{"KiB", 1 << 10},
		{"B", 1},
	}
	upper := strings.ToUpper(s)
	for _, sf := range suffixes {
		if strings.HasSuffix(upper, strings.ToUpper(sf.suffix)) {
			numStr := strings.TrimSuffix(upper, strings.ToUpper(sf.suffix))
			numStr = strings.TrimSpace(numStr)
			// Strict whole-string parse: fmt.Sscanf("%d") used to
			// accept "1.5" as 1 and "12abc" as 12, silently applying
			// the wrong limit. Fractional values are rejected (the
			// caller logs and ignores them) rather than truncated.
			n, err := strconv.ParseInt(numStr, 10, 64)
			if err != nil {
				return 0, fmt.Errorf("parse number %q: %w", numStr, err)
			}
			if n < 0 {
				return 0, fmt.Errorf("negative size %q", s)
			}
			if n > math.MaxInt64/sf.mult {
				return 0, fmt.Errorf("size %q overflows int64", s)
			}
			return n * sf.mult, nil
		}
	}
	// No suffix: treat as raw bytes.
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("unrecognised size format %q", s)
	}
	if n < 0 {
		return 0, fmt.Errorf("negative size %q", s)
	}
	return n, nil
}
