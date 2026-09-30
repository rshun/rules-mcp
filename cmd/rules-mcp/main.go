package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"rules-mcp/internal/server"
)

var version = "0.4.0"

func main() {
	config := flag.String("config", "/etc/rules-mcp/config.json", "configuration file")
	check := flag.Bool("check", false, "validate configuration and local repository without changing rules or contacting remotes")
	showVersion := flag.Bool("version", false, "show version")
	flag.Parse()
	server.Version = version
	if *showVersion {
		fmt.Println(version)
		return
	}
	if runtime.GOOS != "linux" {
		log.Fatal("deployment is supported only on Linux (Debian); build a linux binary")
	}
	c, err := server.LoadConfig(*config)
	if err != nil {
		log.Fatal(err)
	}
	if _, err = exec.LookPath("git"); err != nil {
		log.Fatal("required executable is missing: git")
	}
	a, err := server.New(c)
	if err != nil {
		log.Fatal(err)
	}
	if _, err = a.Execute(context.Background(), "rules_status", nil); err != nil {
		log.Fatal(err)
	}
	if *check {
		fmt.Println("configuration and local repository checks passed; network access not tested")
		return
	}
	s := &http.Server{Addr: c.Listen, Handler: a.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: time.Duration(c.TimeoutSeconds+10) * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan struct{})
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), time.Duration(c.TimeoutSeconds+15)*time.Second)
		defer cancel()
		_ = s.Shutdown(shutdown)
		close(done)
	}()
	log.Printf("rules-mcp %s listening on http://%s/mcp (loopback only)", version, c.Listen)
	if err = s.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal("HTTP server failed to listen")
	}
	<-done
}
