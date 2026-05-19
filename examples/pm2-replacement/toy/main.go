// Minimal HTTP server used by the pm2-replacement example.
// Identity comes from APP_NAME; listens on $PORT.
//
//	GET /        → "hello from <name> (pid=N, host=H)"
//	GET /healthz → 200 OK
//	GET /crash   → exits 1 (used to demo supervisor auto-restart)
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
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/crash", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "crashing")
		go func() { os.Exit(1) }()
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "hello from %s (pid=%d, host=%s)\n",
			name, os.Getpid(), r.Host)
	})

	fmt.Printf("toy %s listening on :%s (pid=%d)\n", name, port, os.Getpid())
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		fmt.Fprintf(os.Stderr, "listen: %v\n", err)
		os.Exit(1)
	}
}
