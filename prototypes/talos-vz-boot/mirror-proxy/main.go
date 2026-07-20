// PROTOTYPE — wayfinder ticket #16 (gate G4): minimal pull-through registry proxy.
// Serves registry.k8s.io to vmnet guests over plain HTTP; all upstream fetches
// (including CDN blob redirects) happen server-side as host-process traffic.
package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
)

const upstream = "https://registry.k8s.io"

func main() {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		req, err := http.NewRequest(r.Method, upstream+r.URL.RequestURI(), nil)
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		for _, h := range []string{"Accept", "Accept-Encoding", "Range"} {
			if v := r.Header.Get(h); v != "" {
				req.Header.Set(h, v)
			}
		}
		resp, err := http.DefaultClient.Do(req) // follows CDN redirects server-side
		if err != nil {
			fmt.Fprintf(os.Stderr, "[proxy] %s %s -> ERROR %v\n", r.Method, r.URL.Path, err)
			http.Error(w, err.Error(), 502)
			return
		}
		defer resp.Body.Close()
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		n, _ := io.Copy(w, resp.Body)
		fmt.Fprintf(os.Stderr, "[proxy] %s %s -> %d (%d bytes)\n", r.Method, r.URL.Path, resp.StatusCode, n)
	})
	fmt.Fprintln(os.Stderr, "[proxy] listening on :5055 for", upstream)
	panic(http.ListenAndServe(":5055", nil))
}
