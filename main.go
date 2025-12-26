package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type WatcherConfig struct {
	Monitor struct {
		DashboardEnabled           bool   `json:"dashboard_enabled"`
		ProgramPath                string `json:"program_path"`
		RestartOnExit              bool   `json:"restart_on_exit"`
		MaxRestartsPerHour         int    `json:"max_restarts_per_hour"`
		HealthCheckIntervalSeconds int    `json:"health_check_interval_seconds"`
		ProcessTimeoutSeconds      int    `json:"process_timeout_seconds"`
		Output                     struct {
			LogFile       string `json:"log_file"`
			JsonlFile     string `json:"jsonl_file"`
			PrettyPrint   bool   `json:"pretty_print"`
			StdoutEnabled bool   `json:"stdout_enabled"`
		} `json:"output"`
	} `json:"monitor"`
}

type MetricPoint struct {
	Timestamp time.Time `json:"timestamp"`
	Cpu       float64   `json:"cpu"`
	Memory    uint64    `json:"memory"`
	Restarts  int       `json:"restarts"`
}

type DaemonStatus struct {
	mu          sync.RWMutex
	Running     bool
	Pid         int
	StartTime   time.Time
	LastError   string
	Restarts    int
	CpuPercent  float64
	MemoryBytes uint64
	History     []MetricPoint
}

var (
	restartCount = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "eth_watchtower_daemon_restarts_total",
		Help: "Total number of daemon restarts",
	})
	daemonStartTime = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "eth_watchtower_daemon_start_timestamp_seconds",
		Help: "Timestamp of the last daemon start",
	})
	daemonUp = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "eth_watchtower_daemon_up",
		Help: "1 if the daemon is running, 0 otherwise",
	})
)

func main() {
	configPath := flag.String("config", "config.json", "Path to shared eth-watch config")
	foreground := flag.Bool("foreground", false, "Run in foreground (default is detached)")
	healthAddr := flag.String("health-addr", ":8080", "Address for health check endpoint")
	flag.Parse()

	if !*foreground && os.Getenv("ETH_WATCHTOWER_BG") != "1" {
		exe, err := os.Executable()
		if err != nil {
			log.Fatalf("failed to determine executable: %v", err)
		}
		cmd := exec.Command(exe, os.Args[1:]...)
		cmd.Env = append(os.Environ(), "ETH_WATCHTOWER_BG=1")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			log.Fatalf("failed to start background process: %v", err)
		}
		log.Printf("eth-watchtower-supervisor started in background (PID: %d)", cmd.Process.Pid)
		os.Exit(0)
	}

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	var logMu sync.Mutex
	var currentLogFile *os.File

	f, err := openLogFile(cfg.Monitor.Output.LogFile)
	if err != nil {
		log.Fatalf("failed to open monitor log: %v", err)
	}
	currentLogFile = f
	defer func() {
		logMu.Lock()
		if currentLogFile != nil {
			currentLogFile.Close()
		}
		logMu.Unlock()
	}()

	log.Println("eth-watchtower-supervisor starting…")

	prometheus.MustRegister(restartCount)
	prometheus.MustRegister(daemonStartTime)
	prometheus.MustRegister(daemonUp)

	status := &DaemonStatus{}
	restartChan := make(chan struct{}, 1)

	var configMu sync.RWMutex
	activeConfig := cfg

	checkAuth := func(w http.ResponseWriter, r *http.Request) bool {
		// Auth removed as it's not in the monitor config
		return true
	}

	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
			configMu.RLock()
			progPath := activeConfig.Monitor.ProgramPath
			interval := activeConfig.Monitor.HealthCheckIntervalSeconds
			configMu.RUnlock()

			status.mu.RLock()
			defer status.mu.RUnlock()

			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(struct {
				Running     bool      `json:"running"`
				Pid         int       `json:"pid"`
				StartTime   time.Time `json:"start_time"`
				LastError   string    `json:"last_error,omitempty"`
				Restarts    int       `json:"restarts"`
				CpuPercent  float64   `json:"cpu_percent"`
				MemoryBytes uint64    `json:"memory_bytes"`
				ProgramPath string    `json:"program_path"`
				Interval    int       `json:"health_check_interval"`
			}{
				Running:     status.Running,
				Pid:         status.Pid,
				StartTime:   status.StartTime,
				LastError:   status.LastError,
				Restarts:    status.Restarts,
				CpuPercent:  status.CpuPercent,
				MemoryBytes: status.MemoryBytes,
				ProgramPath: progPath,
				Interval:    interval,
			})
		})
		mux.HandleFunc("/restart", func(w http.ResponseWriter, r *http.Request) {
			if !checkAuth(w, r) {
				return
			}
			if r.Method != http.MethodPost {
				http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
				return
			}

			status.mu.RLock()
			pid := status.Pid
			running := status.Running
			status.mu.RUnlock()

			if running && pid > 0 {
				if proc, err := os.FindProcess(pid); err == nil {
					_ = proc.Signal(syscall.SIGTERM)
				}
			}

			select {
			case restartChan <- struct{}{}:
			default:
			}

			w.WriteHeader(http.StatusOK)
			w.Write([]byte("Restart triggered"))
		})
		mux.HandleFunc("/reload", func(w http.ResponseWriter, r *http.Request) {
			if !checkAuth(w, r) {
				return
			}
			if r.Method != http.MethodPost {
				http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
				return
			}
			newCfg, err := loadConfig(*configPath)
			if err != nil {
				http.Error(w, fmt.Sprintf("failed to reload config: %v", err), http.StatusInternalServerError)
				return
			}
			configMu.Lock()
			activeConfig = newCfg
			configMu.Unlock()
			w.Write([]byte("Config reloaded"))
		})
		mux.HandleFunc("/dashboard", func(w http.ResponseWriter, r *http.Request) {
			configMu.RLock()
			enabled := activeConfig.Monitor.DashboardEnabled
			configMu.RUnlock()

			if !enabled {
				http.Error(w, "Dashboard is disabled", http.StatusForbidden)
				return
			}

			if !checkAuth(w, r) {
				return
			}
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte(dashboardHTML))
		})
		mux.HandleFunc("/logs", func(w http.ResponseWriter, r *http.Request) {
			if !checkAuth(w, r) {
				return
			}
			linesParam := r.URL.Query().Get("lines")
			filter := r.URL.Query().Get("filter")
			n := 50
			if val, err := strconv.Atoi(linesParam); err == nil && val > 0 {
				n = val
			}

			configMu.RLock()
			logPath := activeConfig.Monitor.Output.LogFile
			pretty := activeConfig.Monitor.Output.PrettyPrint
			configMu.RUnlock()

			lines, err := getLastNLines(logPath, n, filter)
			if pretty && err == nil {
				for i, line := range lines {
					var obj interface{}
					if json.Unmarshal([]byte(line), &obj) == nil {
						if indented, err := json.MarshalIndent(obj, "", "  "); err == nil {
							lines[i] = string(indented)
						}
					}
				}
			}

			response := map[string]interface{}{"lines": lines}
			if err != nil {
				response["error"] = err.Error()
			}
			json.NewEncoder(w).Encode(response)
		})
		mux.HandleFunc("/stats/history", func(w http.ResponseWriter, r *http.Request) {
			if !checkAuth(w, r) {
				return
			}
			status.mu.RLock()
			defer status.mu.RUnlock()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(status.History)
		})
		mux.HandleFunc("/logs/download", func(w http.ResponseWriter, r *http.Request) {
			if !checkAuth(w, r) {
				return
			}
			configMu.RLock()
			logPath := activeConfig.Monitor.Output.LogFile
			configMu.RUnlock()

			if logPath != "" {
				w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", filepath.Base(logPath)))
				http.ServeFile(w, r, logPath)
			}
		})
		mux.Handle("/metrics", promhttp.Handler())
		if err := http.ListenAndServe(*healthAddr, mux); err != nil {
			log.Printf("health check server failed: %v", err)
		}
	}()

	hupChan := make(chan os.Signal, 1)
	signal.Notify(hupChan, syscall.SIGHUP)
	go func() {
		for range hupChan {
			log.Println("Received SIGHUP, reloading config...")
			newCfg, err := loadConfig(*configPath)
			if err != nil {
				log.Printf("failed to reload config: %v", err)
				continue
			}
			configMu.Lock()
			activeConfig = newCfg
			configMu.Unlock()
			log.Println("Config reloaded successfully")

			// Rotate logs
			configMu.RLock()
			logPath := activeConfig.Monitor.Output.LogFile
			retention := 7
			configMu.RUnlock()

			logMu.Lock()
			timestamp := time.Now().Format("20060102-150405")
			backupName := fmt.Sprintf("%s.%s", logPath, timestamp)

			if err := os.Rename(logPath, backupName); err != nil {
				log.Printf("warning: failed to rename log file: %v", err)
			} else {
				log.Printf("Log rotated to %s", backupName)
			}

			newLogFile, err := openLogFile(logPath)
			if err != nil {
				log.Printf("failed to reopen log file: %v", err)
			} else {
				if currentLogFile != nil {
					currentLogFile.Close()
				}
				currentLogFile = newLogFile
			}
			logMu.Unlock()

			cleanupOldLogs(logPath, retention)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	for {
		if ctx.Err() != nil {
			log.Println("Shutdown signal received — watcher stopping.")
			return
		}

		configMu.RLock()
		prog := activeConfig.Monitor.ProgramPath
		out := activeConfig.Monitor.Output
		interval := activeConfig.Monitor.HealthCheckIntervalSeconds
		timeout := activeConfig.Monitor.ProcessTimeoutSeconds
		configMu.RUnlock()

		cmdParts := []string{prog, "-config", *configPath}
		if out.JsonlFile != "" {
			cmdParts = append(cmdParts, "-out", out.JsonlFile)
		}
		if out.LogFile != "" {
			cmdParts = append(cmdParts, "-log", out.LogFile)
		}
		cmd := strings.Join(cmdParts, " ")

		logMu.Lock()
		useLogFile := currentLogFile
		logMu.Unlock()

		var logWriter io.Writer = useLogFile
		if out.StdoutEnabled {
			logWriter = io.MultiWriter(useLogFile, os.Stdout)
		}

		runDaemon(ctx, cmd, logWriter, status, timeout)

		select {
		case <-time.After(time.Duration(interval) * time.Second):
		case <-restartChan:
			log.Println("Manual restart triggered")
		case <-ctx.Done():
			log.Println("Watcher interrupted during backoff.")
			return
		}
	}
}

func loadConfig(path string) (WatcherConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return WatcherConfig{}, fmt.Errorf("failed to open config.json: %v", err)
	}
	defer f.Close()

	var cfg WatcherConfig
	if err := json.NewDecoder(f).Decode(&cfg); err != nil {
		return WatcherConfig{}, fmt.Errorf("failed parsing watcher config: %v", err)
	}

	if cfg.Monitor.ProgramPath == "" {
		cfg.Monitor.ProgramPath = "./eth-watchtower"
	}
	if cfg.Monitor.HealthCheckIntervalSeconds <= 0 {
		cfg.Monitor.HealthCheckIntervalSeconds = 5
	}
	if cfg.Monitor.Output.LogFile == "" {
		cfg.Monitor.Output.LogFile = "eth-watchtower-monitor.log"
	}

	return cfg, nil
}

func openLogFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}
	log.SetOutput(f)
	return f, nil
}

func runDaemon(ctx context.Context, command string, logWriter io.Writer, status *DaemonStatus, timeoutSeconds int) {
	log.Printf("starting daemon: %s", command)
	restartCount.Inc()

	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.Stdout = logWriter
	cmd.Stderr = logWriter

	start := time.Now()

	if err := cmd.Start(); err != nil {
		log.Printf("failed starting daemon: %v", err)
		return
	}

	daemonStartTime.Set(float64(start.Unix()))
	daemonUp.Set(1)

	status.mu.Lock()
	status.Running = true
	status.Restarts++
	status.Pid = cmd.Process.Pid
	status.StartTime = start
	status.LastError = ""
	status.mu.Unlock()

	defer func() {
		daemonUp.Set(0)
		status.mu.Lock()
		status.Running = false
		status.Pid = 0
		status.CpuPercent = 0
		status.MemoryBytes = 0
		// History is preserved to show context after crash/restart
		status.mu.Unlock()
	}()

	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()

	stopMetrics := make(chan struct{})
	defer close(stopMetrics)
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stopMetrics:
				return
			case <-ticker.C:
				cpu, mem, err := getProcessStats(cmd.Process.Pid)
				status.mu.Lock()
				if err == nil {
					status.CpuPercent = cpu
					status.MemoryBytes = mem
					status.History = append(status.History, MetricPoint{
						Timestamp: time.Now(),
						Cpu:       cpu,
						Memory:    mem,
						Restarts:  status.Restarts,
					})
					if len(status.History) > 60 {
						status.History = status.History[len(status.History)-60:]
					}
				}
				status.mu.Unlock()
			}
		}
	}()

	var timeoutCh <-chan time.Time
	if timeoutSeconds > 0 {
		timer := time.NewTimer(time.Duration(timeoutSeconds) * time.Second)
		defer timer.Stop()
		timeoutCh = timer.C
	}

	select {
	case <-ctx.Done():
		log.Println("shutdown requested — stopping daemon")
		_ = cmd.Process.Kill()
		<-wait
	case <-timeoutCh:
		log.Printf("daemon execution timed out after %ds — killing", timeoutSeconds)
		_ = cmd.Process.Kill()
		<-wait
		status.mu.Lock()
		status.LastError = "execution timed out"
		status.mu.Unlock()
	case err := <-wait:
		status.mu.Lock()
		if err != nil {
			status.LastError = err.Error()
			log.Printf("daemon exited with error after %s: %v", time.Since(start), err)
		} else {
			status.LastError = ""
			log.Printf("daemon exited normally after %s", time.Since(start))
		}
		status.mu.Unlock()
	}
}

func cleanupOldLogs(logPath string, retentionDays int) {
	if retentionDays <= 0 {
		return
	}
	dir := filepath.Dir(logPath)
	base := filepath.Base(logPath)

	entries, err := os.ReadDir(dir)
	if err != nil {
		log.Printf("failed to read log directory for cleanup: %v", err)
		return
	}

	cutoff := time.Now().AddDate(0, 0, -retentionDays)

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		// Check if it looks like a rotated log file: base + "." + timestamp
		if strings.HasPrefix(entry.Name(), base+".") {
			info, err := entry.Info()
			if err != nil {
				continue
			}
			if info.ModTime().Before(cutoff) {
				fullPath := filepath.Join(dir, entry.Name())
				if err := os.Remove(fullPath); err != nil {
					log.Printf("failed to remove old log file %s: %v", fullPath, err)
				} else {
					log.Printf("removed old log file: %s", fullPath)
				}
			}
		}
	}
}

func getLastNLines(path string, n int, filter string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return nil, err
	}
	filesize := stat.Size()

	var offset int64 = 0
	const bufSize = 100 * 1024 // Read last 100KB
	if filesize > bufSize {
		offset = filesize - bufSize
	}

	buf := make([]byte, filesize-offset)
	_, err = f.ReadAt(buf, offset)
	if err != nil {
		return nil, err
	}

	lines := strings.Split(string(buf), "\n")
	if offset > 0 && len(lines) > 0 {
		lines = lines[1:]
	}
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	var filtered []string
	lowerFilter := strings.ToLower(filter)
	for _, line := range lines {
		if filter == "" || strings.Contains(strings.ToLower(line), lowerFilter) {
			filtered = append(filtered, line)
		}
	}

	if len(filtered) > n {
		return filtered[len(filtered)-n:], nil
	}
	return filtered, nil
}

func getProcessStats(pid int) (float64, uint64, error) {
	cmd := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "%cpu,rss")
	out, err := cmd.Output()
	if err != nil {
		return 0, 0, err
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return 0, 0, fmt.Errorf("no output")
	}
	fields := strings.Fields(lines[1])
	if len(fields) < 2 {
		return 0, 0, fmt.Errorf("parse error")
	}
	cpu, _ := strconv.ParseFloat(fields[0], 64)
	rss, _ := strconv.ParseFloat(fields[1], 64)
	return cpu, uint64(rss * 1024), nil
}

const dashboardHTML = `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <script src="https://cdn.jsdelivr.net/npm/chart.js"></script>
    <title>ETH Watchtower Supervisor</title>
    <style>
        body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif; background-color: #f4f4f9; color: #333; display: flex; justify-content: center; padding-top: 50px; margin: 0; }
        .dashboard { background: white; padding: 2rem; border-radius: 12px; box-shadow: 0 4px 6px rgba(0,0,0,0.1); width: 100%; max-width: 600px; }
        h1 { margin: 0; font-size: 1.5rem; color: #2c3e50; }
        .metric { display: flex; justify-content: space-between; align-items: center; margin-bottom: 1rem; padding: 0.5rem 0; border-bottom: 1px solid #f0f0f0; }
        .metric:last-child { border-bottom: none; }
        .label { font-weight: 500; color: #666; }
        .value { font-family: "SF Mono", "Monaco", "Inconsolata", "Fira Mono", "Droid Sans Mono", "Source Code Pro", monospace; font-weight: 600; }
        .status-running { color: #27ae60; background: #e8f8f5; padding: 4px 8px; border-radius: 4px; }
        .status-stopped { color: #c0392b; background: #fdedec; padding: 4px 8px; border-radius: 4px; }
        .error-text { color: #c0392b; font-size: 0.9rem; }
        .controls { margin-top: 2rem; display: flex; gap: 10px; }
        button { flex: 1; padding: 10px; border: none; border-radius: 6px; cursor: pointer; font-weight: 600; transition: opacity 0.2s; }
        button:hover { opacity: 0.9; }
        .btn-restart { background-color: #e67e22; color: white; }
        .btn-reload { background-color: #3498db; color: white; }
        .btn-download { background-color: #7f8c8d; color: white; }
        .logs-box { background: #2c3e50; color: #ecf0f1; padding: 10px; border-radius: 6px; height: 250px; overflow-y: auto; font-size: 0.75rem; white-space: pre-wrap; margin-top: 1rem; border: 1px solid #34495e; }
        .tail-box { background: #f8f9fa; padding: 10px; border-radius: 6px; border: 1px solid #e0e0e0; font-size: 0.75rem; white-space: pre-wrap; }
        .filter-input { padding: 4px 8px; border-radius: 4px; border: 1px solid #ccc; font-size: 0.85rem; }
        .log-error { color: #e74c3c; font-weight: bold; }
        .log-warning { color: #e67e22; font-weight: bold; }
        .logs-box .log-error { color: #ff6b6b; }
        .logs-box .log-warning { color: #f1c40f; }

        /* Dark Mode & Header */
        body.dark-mode { background-color: #1a1a1a; color: #f0f0f0; }
        body.dark-mode .dashboard { background-color: #2c2c2c; color: #f0f0f0; }
        body.dark-mode h1 { color: #f0f0f0; }
        body.dark-mode .metric { border-bottom-color: #444; }
        body.dark-mode .label { color: #aaa; }
        body.dark-mode .filter-input { background-color: #444; color: #fff; border-color: #666; }
        body.dark-mode .status-running { background-color: #1e3a2f; }
        body.dark-mode .status-stopped { background-color: #3a1e1e; }
        body.dark-mode .logs-box { background-color: #151515; border-color: #444; }
        body.dark-mode .tail-box { background-color: #151515; border-color: #444; color: #ccc; }
        body.dark-mode .log-error { color: #ff6b6b; }
        body.dark-mode .log-warning { color: #f1c40f; }
        .header-row { display: flex; justify-content: space-between; align-items: center; border-bottom: 2px solid #eee; padding-bottom: 1rem; margin-bottom: 1.5rem; }
        body.dark-mode .header-row { border-bottom-color: #444; }
        .btn-mode { background: none; border: 1px solid #ccc; padding: 4px 8px; border-radius: 4px; cursor: pointer; font-size: 1.2rem; flex: none; width: auto; }
        body.dark-mode .btn-mode { border-color: #666; color: #fff; }
    </style>
</head>
<body>
    <div class="dashboard">
        <div class="header-row">
            <h1>Supervisor Dashboard</h1>
            <button class="btn-mode" onclick="toggleDarkMode()" title="Toggle Dark Mode">🌓</button>
        </div>
        <div class="metric">
            <span class="label">Status</span>
            <span id="status" class="value">...</span>
        </div>
        <div class="metric">
            <span class="label">Program Path</span>
            <span id="program_path" class="value">...</span>
        </div>
        <div class="metric">
            <span class="label">Health Check</span>
            <span id="health_check" class="value">...</span>
        </div>
        <div class="metric">
            <span class="label">PID</span>
            <span id="pid" class="value">...</span>
        </div>
        <div class="metric">
            <span class="label">Uptime</span>
            <span id="uptime" class="value">...</span>
        </div>
        <div class="metric">
            <span class="label">Restarts</span>
            <span id="restarts" class="value">...</span>
        </div>
        <div class="metric">
            <span class="label">CPU Usage</span>
            <span id="cpu" class="value">...</span>
        </div>
        <div class="metric">
            <span class="label">Memory</span>
            <span id="memory" class="value">...</span>
        </div>
        <div style="margin-top: 1rem; height: 200px; position: relative;">
            <canvas id="cpuChart"></canvas>
        </div>
        <div style="margin-top: 1rem; height: 200px; position: relative;">
            <canvas id="memoryChart"></canvas>
        </div>
        <div style="margin-top: 1rem; height: 200px; position: relative;">
            <canvas id="restartsChart"></canvas>
        </div>

        <div class="metric" style="display:block">
            <div style="display:flex; justify-content:space-between; margin-bottom:0.5rem">
                <span class="label">Last Error</span>
            </div>
            <div id="last-error" class="value error-text">None</div>
        </div>

        <div class="metric" style="display:block">
            <div class="label" style="margin-bottom:0.5rem">Log Tail</div>
            <div id="log-tail" class="value tail-box">...</div>
        </div>

        <div style="margin-top: 1.5rem;">
            <div style="display:flex; justify-content:space-between; align-items:center; margin-bottom: 0.5rem;">
                <div class="label">Recent Logs</div>
                <div style="display:flex; gap: 5px; align-items: center;">
                    <label style="font-size: 0.85rem; margin-right: 5px; cursor: pointer;"><input type="checkbox" id="autoscroll" checked> Auto-scroll</label>
                    <input type="text" id="filter" class="filter-input" placeholder="Filter logs..." onkeyup="update()">
                    <button onclick="clearFilter()" style="padding: 4px 8px; font-size: 0.85rem; flex: none; width: auto; background-color: #95a5a6; color: white;">Clear</button>
                </div>
            </div>
            <div id="logs" class="logs-box">Loading logs...</div>
        </div>

        <div class="controls">
            <button class="btn-restart" onclick="trigger('/restart')">Restart Process</button>
            <button class="btn-reload" onclick="trigger('/reload')">Reload Config</button>
            <button class="btn-download" onclick="window.location.href='/logs/download'">Download Logs</button>
        </div>
    </div>

    <script>
        let cpuChart;
        let memoryChart;
        let restartsChart;
        function initChart() {
            const ctxCpu = document.getElementById('cpuChart').getContext('2d');
            cpuChart = new Chart(ctxCpu, {
                type: 'line',
                data: {
                    labels: [],
                    datasets: [{
                        label: 'CPU %',
                        data: [],
                        borderColor: '#e67e22',
                        backgroundColor: 'rgba(230, 126, 34, 0.1)',
                        fill: true,
                        tension: 0.3,
                        pointRadius: 0,
                        borderWidth: 2
                    }]
                },
                options: {
                    responsive: true,
                    maintainAspectRatio: false,
                    animation: false,
                    interaction: { mode: 'index', intersect: false },
                    plugins: { legend: { display: true } },
                    scales: {
                        x: { display: false },
                        y: { type: 'linear', display: true, position: 'left', min: 0, grid: { color: '#eee' } }
                    }
                }
            });

            const ctxMem = document.getElementById('memoryChart').getContext('2d');
            memoryChart = new Chart(ctxMem, {
                type: 'line',
                data: {
                    labels: [],
                    datasets: [{
                        label: 'Memory (MB)',
                        data: [],
                        borderColor: '#3498db',
                        backgroundColor: 'rgba(52, 152, 219, 0.1)',
                        fill: true,
                        tension: 0.3,
                        pointRadius: 0,
                        borderWidth: 2
                    }]
                },
                options: {
                    responsive: true,
                    maintainAspectRatio: false,
                    animation: false,
                    interaction: { mode: 'index', intersect: false },
                    plugins: { legend: { display: true } },
                    scales: {
                        x: { display: false },
                        y: { type: 'linear', display: true, position: 'left', min: 0, grid: { color: '#eee' } }
                    }
                }
            });

            const ctx2 = document.getElementById('restartsChart').getContext('2d');
            restartsChart = new Chart(ctx2, {
                type: 'line',
                data: {
                    labels: [],
                    datasets: [{
                        label: 'Restart Count',
                        data: [],
                        borderColor: '#9b59b6',
                        backgroundColor: 'rgba(155, 89, 182, 0.1)',
                        fill: true,
                        tension: 0.1,
                        pointRadius: 1,
                        borderWidth: 2,
                        stepped: true
                    }]
                },
                options: {
                    responsive: true,
                    maintainAspectRatio: false,
                    animation: false,
                    interaction: { mode: 'index', intersect: false },
                    plugins: { legend: { display: true } },
                    scales: {
                        x: { display: false },
                        y: { type: 'linear', display: true, position: 'left', min: 0, ticks: { stepSize: 1 }, grid: { color: '#eee' } }
                    }
                }
            });
        }

        if (localStorage.getItem('darkMode') === 'true') document.body.classList.add('dark-mode');

        function toggleDarkMode() {
            document.body.classList.toggle('dark-mode');
            localStorage.setItem('darkMode', document.body.classList.contains('dark-mode'));
        }

        function escapeHtml(text) {
            if (!text) return text;
            return text.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;").replace(/'/g, "&#039;");
        }

        function highlightLogs(lines) {
            if (!lines) return '';
            return lines.map(line => {
                let escaped = escapeHtml(line);
                escaped = escaped.replace(/(ERROR|FATAL|CRITICAL)/g, '<span class="log-error">$1</span>');
                escaped = escaped.replace(/(WARNING|WARN)/g, '<span class="log-warning">$1</span>');
                return escaped;
            }).join('\n');
        }

        function update() {
            fetch('/health')
                .then(r => r.json())
                .then(data => {
                    const statusEl = document.getElementById('status');
                    statusEl.textContent = data.running ? 'RUNNING' : 'STOPPED';
                    statusEl.className = 'value ' + (data.running ? 'status-running' : 'status-stopped');
                    document.getElementById('program_path').textContent = data.program_path || '-';
                    document.getElementById('health_check').textContent = (data.health_check_interval || '-') + 's';
                    
                    document.getElementById('pid').textContent = data.pid || '-';
                    document.getElementById('restarts').textContent = data.restarts !== undefined ? data.restarts : '-';
                    document.getElementById('last-error').textContent = data.last_error || 'None';
                    document.getElementById('cpu').textContent = (data.cpu_percent || 0).toFixed(1) + '%';
                    document.getElementById('memory').textContent = formatBytes(data.memory_bytes || 0);

                    if (data.running && data.start_time) {
                        const start = new Date(data.start_time);
                        const diff = Math.floor((new Date() - start) / 1000);
                        const h = Math.floor(diff / 3600);
                        const m = Math.floor((diff % 3600) / 60);
                        const s = diff % 60;
                        document.getElementById('uptime').textContent = h + 'h ' + m + 'm ' + s + 's';
                    } else {
                        document.getElementById('uptime').textContent = '-';
                    }
                })
                .catch(e => console.error('Fetch error:', e));

            const filter = document.getElementById('filter').value;
            fetch('/logs?lines=50&filter=' + encodeURIComponent(filter))
                .then(r => r.json())
                .then(data => {
                    const logsEl = document.getElementById('logs');
                    if (data.error) {
                        logsEl.textContent = 'Error loading logs: ' + data.error;
                    } else if (data.lines) {
                        logsEl.innerHTML = highlightLogs(data.lines);
                        if (document.getElementById('autoscroll').checked) {
                            logsEl.scrollTop = logsEl.scrollHeight;
                        }
                    }
                })
                .catch(e => console.error('Fetch error:', e));

            fetch('/logs?lines=10')
                .then(r => r.json())
                .then(data => {
                    const tailEl = document.getElementById('log-tail');
                    if (data.lines) tailEl.innerHTML = highlightLogs(data.lines);
                })
                .catch(e => console.error('Fetch error:', e));

            fetch('/stats/history')
                .then(r => r.json())
                .then(data => {
                    if (!data) return;
                    const labels = data.map(d => new Date(d.timestamp).toLocaleTimeString());
                    const cpuData = data.map(d => d.cpu);
                    const memData = data.map(d => (d.memory / 1024 / 1024).toFixed(1));
                    const restartData = data.map(d => d.restarts);

                    if (cpuChart) {
                        cpuChart.data.labels = labels;
                        cpuChart.data.datasets[0].data = cpuData;
                        cpuChart.update();
                    }
                    if (memoryChart) {
                        memoryChart.data.labels = labels;
                        memoryChart.data.datasets[0].data = memData;
                        memoryChart.update();
                    }
                    if (restartsChart) {
                        restartsChart.data.labels = labels;
                        restartsChart.data.datasets[0].data = restartData;
                        restartsChart.update();
                    }
                });
        }

        function trigger(endpoint) {
            if(!confirm('Are you sure?')) return;
            fetch(endpoint, { method: 'POST' })
                .then(r => r.text())
                .then(alert)
                .catch(alert);
        }

        function clearFilter() {
            document.getElementById('filter').value = '';
            update();
        }

        function formatBytes(bytes, decimals = 2) {
            if (!+bytes) return '0 B';
            const k = 1024;
            const dm = decimals < 0 ? 0 : decimals;
            const sizes = ['B', 'KB', 'MB', 'GB', 'TB'];
            const i = Math.floor(Math.log(bytes) / Math.log(k));
            return parseFloat((bytes / Math.pow(k, i)).toFixed(dm)) + ' ' + sizes[i];
        }

        initChart();
        update();
        setInterval(update, 2000);
    </script>
</body>
</html>`
