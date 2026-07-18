package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	client := defaultHTTPClient()
	store, err := newTokenStore(defaultAuthPath(), client)
	if err != nil {
		return err
	}
	command := "serve"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch command {
	case "login":
		return interactiveLogin(ctx, store)
	case "logout":
		if err := store.logout(); err != nil {
			return err
		}
		fmt.Println("Logged out.")
		return nil
	case "serve":
		if !store.authenticated() {
			fmt.Println("No saved login found; starting interactive login.")
			if err := interactiveLogin(ctx, store); err != nil {
				return err
			}
		}
		addr := env("CHATGPT_ADAPTER_ADDR", "127.0.0.1:8080")
		apiKey := os.Getenv("CHATGPT_ADAPTER_API_KEY")
		if err := safeListenAddress(addr, apiKey); err != nil {
			return err
		}
		server := &http.Server{
			Addr:              addr,
			Handler:           newProxyServer(store, client, apiKey).routes(),
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       5 * time.Minute,
			IdleTimeout:       2 * time.Minute,
		}
		go func() {
			<-ctx.Done()
			shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = server.Shutdown(shutdown)
		}()
		log.Printf("OpenAI-compatible proxy listening on http://%s", addr)
		err := server.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	default:
		return fmt.Errorf("unknown command %q (use serve, login, or logout)", command)
	}
}
