// Entry lifecycle: bind an ephemeral port, tell the env which port it is,
// then poll until the env says the trial is over and exit 0.
//
// The suite scores the server in this binary; this file only runs the
// harness protocol around it:
//
//  1. MAKEFUN_ENV_URL names the env origin (http://store:8080 in a trial
//     container). With no env to talk to, the process serves standalone on
//     an ephemeral port, prints its URL, and keeps running (local testing).
//  2. Listen on port 0 and let the kernel pick a free port. Never bind
//     8080: the env owns it.
//  3. POST $MAKEFUN_ENV_URL/register with {"port": N}. The env records the
//     address the request came from plus N as the base URL and dials it
//     from then on. The first register wins; a 409 means another entry
//     already registered, and this process keeps waiting like any other.
//  4. GET $MAKEFUN_ENV_URL/done until the body is {"done": true}. The env
//     answers at once ({"done": false} while the suite runs), so this is a
//     poll, not a long-held request. A failed or slow poll is retried with
//     a short exponential backoff; the process never exits before the env
//     says done.
//  5. Close the server and exit 0. The harness then stops the env, which
//     writes the export the scorer reads.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const requestTimeout = 30 * time.Second
const retryStart = 250 * time.Millisecond
const retryCap = 2 * time.Second

func backoffMs(failures int) time.Duration {
	if failures > 8 {
		failures = 8
	}
	d := retryStart * time.Duration(1<<uint(failures))
	if d > retryCap {
		d = retryCap
	}
	return d
}

var httpClient = &http.Client{Timeout: requestTimeout}

func registerPort(envOrigin string, port int) error {
	body, _ := json.Marshal(map[string]any{"port": port})
	for failures := 0; ; failures++ {
		resp, err := httpClient.Post(envOrigin+"/register", "application/json", bytes.NewReader(body))
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 || resp.StatusCode == 409 {
				return nil
			}
			if resp.StatusCode == 400 || resp.StatusCode == 405 {
				return fmt.Errorf("register answered HTTP %d", resp.StatusCode)
			}
		}
		time.Sleep(backoffMs(failures))
	}
}

func waitUntilDone(envOrigin string) {
	for failures := 0; ; failures++ {
		resp, err := httpClient.Get(envOrigin + "/done")
		if err == nil {
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				var parsed struct {
					Done bool `json:"done"`
				}
				if json.Unmarshal(raw, &parsed) == nil && parsed.Done {
					return
				}
			}
		}
		time.Sleep(backoffMs(failures))
	}
}

func main() {
	envOrigin := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")

	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[entry] listen: %v\n", err)
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	server := newServer()

	go func() {
		if err := server.Serve(ln); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "[entry] serve: %v\n", err)
			os.Exit(1)
		}
	}()
	fmt.Printf("[entry] listening on port %d\n", port)

	if envOrigin == "" {
		// Standalone mode (local testing): print the URL and serve forever.
		fmt.Printf("FORGE_URL=http://127.0.0.1:%d\n", port)
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
		<-stop
		_ = server.Close()
		return
	}

	if err := registerPort(envOrigin, port); err != nil {
		fmt.Fprintf(os.Stderr, "[entry] %v\n", err)
		os.Exit(1)
	}
	fmt.Println("[entry] registered; waiting for the env to say done")

	waitUntilDone(envOrigin)
	_ = server.Close()
	fmt.Println("[entry] done")
}
