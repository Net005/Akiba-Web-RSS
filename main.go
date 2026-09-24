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

func defaultConfigPath() string {
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), "config.json")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "config.json"
}

func main() {
	cfgPath := flag.String("config", defaultConfigPath(), "path to config.json")
	showVer := flag.Bool("version", false, "print version and exit")
	health := flag.Bool("healthcheck", false, "probe the local /health endpoint and exit 0/1 (for Docker HEALTHCHECK)")
	flag.Parse()
	if *showVer {
		fmt.Println("akiba-web-rss", version)
		return
	}
	abs, _ := filepath.Abs(*cfgPath)
	if *health {
		os.Exit(healthcheck(abs))
	}

	log := NewHub(3000)
	cfg, err := LoadConfig(abs)
	if err != nil {
		log.Error("%v", err)
		os.Exit(1)
	}
	c := cfg.Get()
	if err := log.OpenFile(c.LogFile); err != nil {
		log.Error("cannot open log file %s: %v", c.LogFile, err)
	}
	log.Info("Akiba-Web RSS %s starting (config: %s)", version, abs)

	app := NewApp(cfg, log)
	if err := app.st.Load(); err != nil {
		log.Error("State load failed: %v", err)
	}
	app.loadCaches()

	if c.WebUser == "" {
		log.Warn("Control panel has NO authentication. Set web_user / web_password in config.json if this host is reachable by others.")
	}

	srv := NewServer(app)
	addr := net.JoinHostPort(c.ListenHost, fmt.Sprint(c.Port))
	httpSrv := &http.Server{Addr: addr, Handler: srv.Handler(), ReadHeaderTimeout: 15 * time.Second}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Error("cannot listen on %s: %v", addr, err)
		os.Exit(1)
	}
	go func() {
		log.Info("Control panel + RSS on http://%s  (feed: /giga/feed)", addr)
		if err := httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Error("HTTP server: %v", err)
		}
	}()

	// Background: connect to MyJD, first scrape, then the scheduler.
	go func() {
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
func healthcheck(cfgPath string) int {
	port := 5000
	if cs, err := LoadConfig(cfgPath); err == nil {
		port = cs.Get().Port
	}
	c := http.Client{Timeout: 4 * time.Second}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/health", port))
	if err != nil || resp.StatusCode != 200 {
		return 1
	}
	return 0
}
