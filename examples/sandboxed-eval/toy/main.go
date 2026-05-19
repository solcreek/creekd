// Toy program for the sandboxed-eval example. Pretends to be a
// user-submitted script: prints what it can see, optionally
// allocates a lot of memory to trip the cgroup cap.
//
//	GET /              → 'hello, I am pid=N, hostname=H, uid=U'
//	GET /view          → list / and /etc contents — proves chroot
//	GET /alloc?mb=N    → allocates N MiB, touches every page (forces RSS)
//	GET /healthz       → 200
package main

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
)

var hog [][]byte // global so allocations aren't GC'd

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	host, _ := os.Hostname()

	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "hello, I am pid=%d hostname=%s uid=%d\n",
			os.Getpid(), host, os.Getuid())
	})

	mux.HandleFunc("/view", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "=== / ===")
		listDir(w, "/")
		fmt.Fprintln(w, "=== /etc ===")
		listDir(w, "/etc")
	})

	mux.HandleFunc("/alloc", func(w http.ResponseWriter, r *http.Request) {
		mb, _ := strconv.Atoi(r.URL.Query().Get("mb"))
		if mb <= 0 {
			http.Error(w, "?mb=N required", 400)
			return
		}
		buf := make([]byte, mb<<20)
		for i := 0; i < len(buf); i += 4096 {
			buf[i] = 1
		}
		hog = append(hog, buf)
		fmt.Fprintf(w, "allocated %d MiB (cumulative %d entries)\n", mb, len(hog))
	})

	if err := http.ListenAndServe(":"+port, mux); err != nil {
		fmt.Fprintf(os.Stderr, "listen: %v\n", err)
		os.Exit(1)
	}
}

func listDir(w http.ResponseWriter, dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		fmt.Fprintf(w, "  (cannot read %s: %v)\n", dir, err)
		return
	}
	for _, e := range entries {
		kind := "f"
		if e.IsDir() {
			kind = "d"
		}
		fmt.Fprintf(w, "  %s %s\n", kind, e.Name())
	}
}
