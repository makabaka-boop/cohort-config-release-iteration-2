// Command server 是配置发布服务的 HTTP 入口。
//
// 特殊用法：
//
//	/server -healthcheck
//
// 对本地 HTTP_ADDR 的 /healthz 发一次探测，供容器 healthcheck 使用
// （distroless 镜像内没有 shell / curl / wget）。
package main

import (
	"context"
	"database/sql"
	"flag"
	"log"
	"net/http"
	"os"
	"time"

	_ "github.com/lib/pq"

	"configrelease/internal/api"
	"configrelease/internal/resolve"
	"configrelease/internal/store"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	healthcheck := flag.Bool("healthcheck", false,
		"probe the local /healthz endpoint once and exit (for container healthcheck)")
	flag.Parse()

	addr := env("HTTP_ADDR", ":8080")

	if *healthcheck {
		os.Exit(probeHealth(addr))
	}

	dsn := env("DATABASE_DSN",
		"postgres://postgres:postgres@db:5432/configdb?sslmode=disable")
	if err := run(dsn, addr); err != nil {
		log.Fatal(err)
	}
}

func run(dsn, addr string) error {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(30 * time.Minute)

	// Compose 已通过 healthcheck 排序，这里仍容忍数据库短暂未就绪。
	if err := waitForDB(db, 30*time.Second); err != nil {
		log.Fatalf("db never became ready: %v", err)
	}

	st := store.New(db)
	if err := st.Migrate(context.Background()); err != nil {
		log.Fatalf("migrate: %v", err)
	}

	rs := resolve.New(st)
	srv := api.NewServer(st, rs)

	log.Printf("config-release server listening on %s", addr)
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           withLogging(srv),
		ReadHeaderTimeout: 10 * time.Second,
	}
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	return nil
}

// probeHealth 请求本地 /healthz；成功返回 0，否则返回 1。
func probeHealth(addr string) int {
	host := addr
	if len(host) > 0 && host[0] == ':' {
		host = "127.0.0.1" + host
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + host + "/healthz")
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func waitForDB(db *sql.DB, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		lastErr = db.PingContext(ctx)
		cancel()
		if lastErr == nil {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return lastErr
}

func withLogging(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		h.ServeHTTP(sw, r)
		log.Printf("%s %s -> %d (%s)", r.Method, r.URL.RequestURI(), sw.status, time.Since(start))
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}
