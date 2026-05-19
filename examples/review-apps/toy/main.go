// Toy for the review-apps example. APP_NAME identifies the branch
// or PR; APP_VERSION identifies the commit/build. The response
// surfaces both so you can see version swaps in real time.
package main

import (
	"fmt"
	"net/http"
	"os"
)

func main() {
	name := os.Getenv("APP_NAME")
	if name == "" {
		name = "anonymous"
	}
	version := os.Getenv("APP_VERSION")
	if version == "" {
		version = "untagged"
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "review app: %s (version %s, host=%s, pid=%d)\n",
			name, version, r.Host, os.Getpid())
	})

	if err := http.ListenAndServe(":"+port, mux); err != nil {
		fmt.Fprintf(os.Stderr, "listen: %v\n", err)
		os.Exit(1)
	}
}
