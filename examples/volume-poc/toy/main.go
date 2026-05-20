// Toy service for the volume-poc demo. Reads/writes a marker file
// under /data so the PoC can prove the bind mount works end-to-end
// and the data persists across process restarts.
package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "18200"
	}
	// DATA_DIR is the path the toy reads/writes through. In the PoC
	// it's /data (the VolumeMount target inside the chroot); set
	// DATA_DIR to override for local smoke testing.
	dataDir := os.Getenv("DATA_DIR")
	if dataDir == "" {
		dataDir = "/data"
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		log.Fatalf("mkdir %s: %v", dataDir, err)
	}

	mux := http.NewServeMux()

	// GET / — health check + identity.
	mux.HandleFunc("GET /", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "volume-poc toy: pid=%d data_dir=%s\n", os.Getpid(), dataDir)
	})

	// POST /write?content=... — write the marker.
	mux.HandleFunc("POST /write", func(w http.ResponseWriter, r *http.Request) {
		content := r.URL.Query().Get("content")
		if content == "" {
			http.Error(w, "?content= required", 400)
			return
		}
		path := filepath.Join(dataDir, "marker")
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		fmt.Fprintf(w, "wrote %d bytes to %s\n", len(content), path)
	})

	// GET /read — read the marker.
	mux.HandleFunc("GET /read", func(w http.ResponseWriter, _ *http.Request) {
		body, err := os.ReadFile(filepath.Join(dataDir, "marker"))
		if err != nil {
			if os.IsNotExist(err) {
				http.Error(w, "marker not yet written", 404)
				return
			}
			http.Error(w, err.Error(), 500)
			return
		}
		_, _ = io.Copy(w, eqStringReader(string(body)))
	})

	// GET /probe-host?path=/etc/passwd — attempt to read a path
	// OUTSIDE the volume. Used by the attack matrix to prove the
	// sandbox + chroot keep the tenant inside its view.
	mux.HandleFunc("GET /probe-host", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Query().Get("path")
		if path == "" {
			http.Error(w, "?path= required", 400)
			return
		}
		body, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(w, "blocked: %v\n", err)
			return
		}
		// Truncate to avoid dumping huge files.
		if len(body) > 256 {
			body = body[:256]
		}
		fmt.Fprintf(w, "LEAK: read %d bytes from %s:\n%s\n", len(body), path, body)
	})

	log.Printf("toy listening on :%s, data_dir=%s", port, dataDir)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatalf("listen: %v", err)
	}
}

// eqStringReader returns a Reader over s without any imports beyond
// stdlib. Used so /read returns raw bytes without a trailing newline.
func eqStringReader(s string) io.Reader {
	return &stringReader{s: s}
}

type stringReader struct {
	s string
	i int
}

func (r *stringReader) Read(p []byte) (int, error) {
	if r.i >= len(r.s) {
		return 0, io.EOF
	}
	n := copy(p, r.s[r.i:])
	r.i += n
	return n, nil
}
