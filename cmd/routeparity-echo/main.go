// Command routeparity-echo is the stand-in backend used in lab runs. It
// listens on one port per Service it impersonates and answers every request
// with a JSON description of what it received.
//
//	routeparity-echo --listen demo/web:8000=:8000 --listen demo/api:http=:8001
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/asaleem9/routeparity/internal/echo"
)

type listener struct {
	id   echo.Identity
	addr string
}

type listenFlag []listener

func (l *listenFlag) String() string { return fmt.Sprintf("%d listener(s)", len(*l)) }

// Set parses NAMESPACE/SERVICE:PORT=ADDRESS.
func (l *listenFlag) Set(value string) error {
	spec, addr, ok := strings.Cut(value, "=")
	if !ok || addr == "" {
		return fmt.Errorf("want NAMESPACE/SERVICE:PORT=ADDRESS, got %q", value)
	}
	namespace, rest, ok := strings.Cut(spec, "/")
	if !ok || namespace == "" {
		return fmt.Errorf("want NAMESPACE/SERVICE:PORT=ADDRESS, got %q", value)
	}
	service, port, ok := strings.Cut(rest, ":")
	if !ok || service == "" || port == "" {
		return fmt.Errorf("want NAMESPACE/SERVICE:PORT=ADDRESS, got %q", value)
	}
	*l = append(*l, listener{id: echo.Identity{Namespace: namespace, Service: service, Port: port}, addr: addr})
	return nil
}

func main() {
	var listeners listenFlag
	flag.Var(&listeners, "listen", "NAMESPACE/SERVICE:PORT=ADDRESS to impersonate a Service port on an address (repeatable)")
	admin := flag.String("admin", ":9090", "address for the health endpoint, kept off the data ports so that no data path is reserved")
	flag.Parse()

	if len(listeners) == 0 {
		log.Fatal("routeparity-echo: at least one --listen is required")
	}
	if err := run(listeners, *admin, os.Getenv("POD_NAME")); err != nil {
		log.Fatalf("routeparity-echo: %v", err)
	}
}

func run(listeners []listener, adminAddr, pod string) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	servers := make([]*http.Server, 0, len(listeners)+1)
	for _, l := range listeners {
		servers = append(servers, &http.Server{
			Addr:              l.addr,
			Handler:           echo.Handler(l.id, pod),
			ReadHeaderTimeout: 10 * time.Second,
		})
	}
	health := http.NewServeMux()
	health.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	servers = append(servers, &http.Server{Addr: adminAddr, Handler: health, ReadHeaderTimeout: 10 * time.Second})

	group, ctx := errgroup.WithContext(ctx)
	for _, srv := range servers {
		group.Go(func() error {
			if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				return fmt.Errorf("listen on %s: %w", srv.Addr, err)
			}
			return nil
		})
	}
	group.Go(func() error {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, srv := range servers {
			_ = srv.Shutdown(shutdownCtx)
		}
		return nil
	})
	return group.Wait()
}
