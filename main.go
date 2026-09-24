// Akiba-Web RSS — Go port with a web control & monitor panel.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

var version = "dev"

// resolveDataDir decides where settings.json, auth.json, state, feed and logs
// live: -data, or the directory of a legacy -config path, or $AKIBA_DATA, or
// (if it holds settings.json / config.json) the executable's directory,
// otherwise the current directory.
func resolveDataDir(dataFlag, cfgFlag string) string {
	switch {
	case dataFlag != "":
		return dataFlag
	case cfgFlag != "":
		return filepath.Dir(cfgFlag)
	case os.Getenv("AKIBA_DATA") != "":
		return os.Getenv("AKIBA_DATA")
	}
	if exe, err := os.Executable(); err == nil {
		d := filepath.Dir(exe)
		for _, n := range []string{settingsName, legacyConfigName} {
			if _, err := os.Stat(filepath.Join(d, n)); err == nil {
				return d
			}
		}
	}
	return "."
}

func main() {
	dataFlag := flag.String("data", "", "data directory (settings.json, auth.json, state, feed, logs)")
	cfgFlag := flag.String("config", "", "legacy: path of an old config.json (its directory becomes the data directory)")
	showVer := flag.Bool("version", false, "print version and exit")
	health := flag.Bool("healthcheck", false, "probe the local /health endpoint and exit 0/1 (for Docker HEALTHCHECK)")
	flag.Parse()
	if *showVer {
		fmt.Println("akiba-web-rss", version)
		return
	}
	dir, _ := filepath.Abs(resolveDataDir(*dataFlag, *cfgFlag))
	if *health {
		os.Exit(healthcheck(dir))
	}
	_ = os.MkdirAll(dir, 0o755)

	log := NewHub(3000)
	log.Info("Akiba-Web RSS %s starting (data directory: %s)", version, dir)

	imported, err := ImportLegacyConfig(dir, log)
	if err != nil {
		log.Error("%v", err)
	}
	cfg, err := LoadConfig(dir)
	if err != nil {
		log.Error("%v", err)
		os.Exit(1)
	}
	c := cfg.Get()
	if err := log.OpenFile(c.LogFile); err != nil {
		log.Error("cannot open log file %s: %v", c.LogFile, err)
	}

	app := NewApp(cfg, log)
	app.Imported = imported
	app.loadState()
	app.loadCaches()

	srv := NewServer(app)
	addr := net.JoinHostPort(c.ListenHost, fmt.Sprint(c.Port))
	httpSrv := &http.Server{Addr: addr, Handler: srv.Handler(), ReadHeaderTimeout: 15 * time.Second}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Error("cannot listen on %s: %v", addr, err)
		os.Exit(1)
	}
	go func() {
		log.Info("Control panel + RSS on http://%s  (feed: /giga/feed — no login needed)", addr)
		if err := httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Error("HTTP server: %v", err)
		}
	}()
	if app.setupCode != "" {
		log.Warn("═══ FIRST-RUN SETUP ═══ open http://<this-host>:%d/setup and enter setup code:  %s", c.Port, app.setupCode)
	}

	// Background: wait for setup (unless settings already exist), connect to
	// MyJD, first scrape, then the scheduler.
	go func() {
		app.WaitConfigured()
		app.InitMyJD()
		app.RunPipeline("startup")
		app.SchedulerLoop()
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Info("Shutting down…")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
}

// healthcheck is used by the container HEALTHCHECK: no shell/curl needed.
func healthcheck(dir string) int {
	port := 5000
	if cs, err := LoadConfig(dir); err == nil {
		port = cs.Get().Port
	}
	c := http.Client{Timeout: 4 * time.Second}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/health", port))
	if err != nil || resp.StatusCode != 200 {
		return 1
	}
	return 0
}
