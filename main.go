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
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "-h", "--h", "--help", "help":
			if len(os.Args) != 2 {
				return fmt.Errorf("help does not accept arguments")
			}
			printHelp()
			return nil
		}
	}

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
	case "usage":
		return codexUsage(ctx, store)
	case "status":
		if len(os.Args) != 2 {
			return fmt.Errorf("status does not accept arguments")
		}
		return codexStatus(ctx, store)
	case "info":
		return codexInfo(ctx, store)
	case "resets":
		if len(os.Args) != 2 {
			return fmt.Errorf("resets does not accept arguments")
		}
		return codexResets(ctx, store)
	case "reset":
		if len(os.Args) < 3 {
			return codexResets(ctx, store)
		}
		if len(os.Args) > 3 {
			return fmt.Errorf("reset accepts at most one reset ID")
		}
		return codexReset(ctx, store, os.Args[2])
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
		return fmt.Errorf("unknown command %q (run `%s --help` for usage)", command, progName())
	}
}

func printHelp() {
	fmt.Printf(`Usage: %s [command]

OpenAI-compatible local proxy backed by a ChatGPT subscription.

Commands:
  serve                 Start the proxy (default command).
  login                 Sign in to ChatGPT through a browser or device code.
  logout                Remove the saved ChatGPT credentials.
  usage                 Show the weekly Codex rate-limit usage.
  status                Show account and Codex rate-limit status.
  info                  Show saved session and access-token details.
  resets                List banked rate-limit reset credits.
  reset [reset-id]      List credits, or immediately consume this exact credit.

Options:
  -h, --h, --help       Show this help text.

The reset command never selects a credit automatically. Run "resets" first,
then pass its complete ID to "reset <reset-id>" to consume that limited credit.

Environment:
  CHATGPT_ADAPTER_ADDR         Proxy listen address (default 127.0.0.1:8080).
  CHATGPT_ADAPTER_API_KEY      Optional required Bearer token for proxy clients.
  CHATGPT_ADAPTER_AUTH_FILE    Credential file path.
  CHATGPT_ADAPTER_SESSION_ID   Default prompt-cache/WebSocket session key.
`, progName())
}
