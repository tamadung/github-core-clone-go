// main.go — the Go forge's entrypoint: the trial lifecycle plus the server.
//
// Lifecycle (the same contract the starter main.ts settles): bind an
// ephemeral port, POST {"port": N} to $MAKEFUN_ENV_URL/register, then poll
// GET $MAKEFUN_ENV_URL/done until it answers {"done": true} and exit 0.
// Never bind 8080 (the env owns it). FORGE_PORT is a local-testing override
// only; the trial never sets it, so the trial always gets an ephemeral port.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

const requestTimeout = 30 * time.Second
const retryInitial = 250 * time.Millisecond
const retryMax = 2 * time.Second

func retryDelay(attempt int) time.Duration {
	if attempt > 8 {
		attempt = 8
	}
	d := retryInitial * time.Duration(1<<uint(attempt))
	if d > retryMax {
		return retryMax
	}
	return d
}

func sleep(d time.Duration) { time.Sleep(d) }

// register posts the bound port until the env accepts it. 200 or 409 ends
// it; 400/405 are contract errors and end the program; anything else is
// retried, since the env may still be starting.
func register(envURL string, port int) {
	client := &http.Client{Timeout: requestTimeout}
	body, _ := json.Marshal(map[string]int{"port": port})
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequest("POST", envURL+"/register", bytes.NewReader(body))
		if err == nil {
			req.Header.Set("content-type", "application/json")
			resp, err2 := client.Do(req)
			if err2 == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode == 200 || resp.StatusCode == 409 {
					return
				}
				if resp.StatusCode == 400 || resp.StatusCode == 405 {
					fatalf("register answered HTTP %d", resp.StatusCode)
				}
			}
		}
		sleep(retryDelay(attempt))
	}
}

// waitForDone polls GET /done until it answers {"done": true}; it never
// returns before that.
func waitForDone(envURL string) {
	client := &http.Client{Timeout: requestTimeout}
	for attempt := 0; ; attempt++ {
		resp, err := client.Get(envURL + "/done")
		if err == nil {
			var parsed struct {
				Done bool `json:"done"`
			}
			if json.NewDecoder(resp.Body).Decode(&parsed) == nil && parsed.Done {
				resp.Body.Close()
				return
			}
			resp.Body.Close()
		}
		sleep(retryDelay(attempt))
	}
}

func fatalf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "[entry] "+format+"\n", args...)
	os.Exit(1)
}

func main() {
	envURL := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	if envURL == "" {
		fatalf("MAKEFUN_ENV_URL is not set: there is no env to register with")
	}
	store := newStore()
	srv := newServer(store)

	var ln net.Listener
	var err error
	if fixed := os.Getenv("FORGE_PORT"); fixed != "" {
		ln, err = net.Listen("tcp", "127.0.0.1:"+fixed)
	} else {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	fmt.Printf("[entry] listening on port %d\n", port)
	go srv.serve(ln)

	register(envURL, port)
	fmt.Printf("[entry] registered; waiting for the env to say done\n")
	waitForDone(envURL)
	fmt.Printf("[entry] done\n")
	os.Exit(0)
}
