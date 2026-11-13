package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/lib/pq"
	"github.com/mattermost/enterprise/push_proxy"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}

func run(args []string) error {
	confirmRotation := len(args) == 1 && args[0] == "confirm-rotation"
	if len(args) != 0 && !confirmRotation {
		return errors.New("usage: workspace-push-gateway [confirm-rotation]")
	}
	listen := os.Getenv("WORKSPACE_PUSH_GATEWAY_LISTEN")
	upstreamValue := os.Getenv("WORKSPACE_PUSH_GATEWAY_UPSTREAM")
	dsn := os.Getenv("WORKSPACE_PUSH_GATEWAY_DATABASE_URL")
	encodedKey := os.Getenv("WORKSPACE_PUSH_GATEWAY_KEY")
	if dsn == "" || encodedKey == "" || (!confirmRotation && (listen == "" || upstreamValue == "")) {
		return errors.New("push gateway requires DATABASE_URL and KEY, and LISTEN and UPSTREAM when serving")
	}
	key, err := base64.RawURLEncoding.DecodeString(encodedKey)
	if err != nil || len(key) != 32 {
		return errors.New("push gateway KEY must be a base64url-encoded 32-byte key")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return fmt.Errorf("open push gateway database: %w", err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err = db.PingContext(ctx); err != nil {
		return fmt.Errorf("connect push gateway database: %w", err)
	}
	store, err := push_proxy.NewSQLTokenStore(db, key)
	if err != nil {
		return err
	}
	if err = store.Init(ctx); err != nil {
		return err
	}
	if _, err = store.Current(ctx); err != nil {
		return fmt.Errorf("verify current push token before starting gateway: %w", err)
	}
	if confirmRotation {
		return store.ConfirmRotation(ctx)
	}
	upstream, err := url.Parse(upstreamValue)
	if err != nil {
		return errors.New("push gateway UPSTREAM is invalid")
	}
	gateway, err := push_proxy.NewGateway(upstream, store)
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr:              listen,
		Handler:           gateway,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	shutdownCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	listenerErr := make(chan error, 1)
	go func() { listenerErr <- server.ListenAndServe() }()
	select {
	case err = <-listenerErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-shutdownCtx.Done():
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err = server.Shutdown(ctx); err != nil {
			return err
		}
		return nil
	}
}
