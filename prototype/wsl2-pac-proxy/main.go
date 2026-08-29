// PROTOTYPE — THROWAWAY. Answers issue #495: can a Windows browser reach a
// WSL2-hosted cluster by name through a PAC file plus an HTTP CONNECT proxy,
// with no Windows route, no NRPT rule and no hosts entry?
//
// Stands in for the real thing decided in #462, which will live inside tbxd and
// resolve in-process via the global lookup closure. Here we resolve over UDP to
// the cluster bridge gateway instead, which is behaviourally identical from
// Windows' point of view and needs no tbxd changes.
//
// Not production code: no tests, no graceful shutdown, no error taxonomy.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

var (
	addr     = flag.String("addr", "127.0.0.1:5390", "proxy listen address")
	resolver = flag.String("resolver", "172.30.0.1:53", "cluster gateway resolver (stands in for tbxd in-process lookup)")
	suffix   = flag.String("suffix", ".k8s.test", "cluster domain suffix")
)

var clusterNet = &net.IPNet{IP: net.IPv4(172, 30, 0, 0), Mask: net.CIDRMask(16, 32)}

// res resolves only against the cluster gateway, never the system resolver —
// WSL's generated resolv.conf points at the Windows NAT DNS proxy and cannot
// answer for cluster names (getent and dig both fail; dig @172.30.0.1 works).
var res = &net.Resolver{
	PreferGo: true,
	Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "udp", *resolver)
	},
}

// allowed is the whole security model: the proxy answers only for names the
// cluster resolver owns, or IP literals inside the cluster supernet. It is
// structurally incapable of being an open proxy.
func allowed(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return clusterNet.Contains(ip)
	}
	return strings.HasSuffix(strings.ToLower(host), *suffix)
}

func resolve(ctx context.Context, host string) (string, error) {
	if ip := net.ParseIP(host); ip != nil {
		return ip.String(), nil
	}
	ips, err := res.LookupIP(ctx, "ip4", host)
	if err != nil {
		return "", err
	}
	if len(ips) == 0 {
		return "", fmt.Errorf("no A record for %s", host)
	}
	return ips[0].String(), nil
}

func split(hostport, defPort string) (string, string) {
	if h, p, err := net.SplitHostPort(hostport); err == nil {
		return h, p
	}
	return hostport, defPort
}

const pac = `function FindProxyForURL(url, host) {
  if (shExpMatch(host, "*%s"))   { return "PROXY %s"; }
  if (shExpMatch(host, "172.30.*")) { return "PROXY %s"; }
  return "DIRECT";
}
`

func main() {
	flag.Parse()
	log.SetFlags(log.Ltime)
	log.SetOutput(io.MultiWriter(os.Stdout))

	var mu sync.Mutex
	seen := map[string]int{}
	note := func(kind, host, detail string) {
		mu.Lock()
		seen[kind]++
		mu.Unlock()
		log.Printf("%-8s %-45s %s", kind, host, detail)
	}

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Origin-form request => we are being asked for the PAC or status,
		// not to proxy. One port serves both roles (decision 2 of #462).
		if r.Method != http.MethodConnect && !r.URL.IsAbs() {
			switch r.URL.Path {
			case "/tbx.pac":
				note("PAC", r.RemoteAddr, "served proxy.pac")
				w.Header().Set("Content-Type", "application/x-ns-proxy-autoconfig")
				fmt.Fprintf(w, pac, *suffix, *addr, *addr)
			default:
				mu.Lock()
				defer mu.Unlock()
				fmt.Fprintf(w, "tbx PAC proxy PROTOTYPE (#495)\nlisten=%s resolver=%s suffix=%s\ncounters=%v\n", *addr, *resolver, *suffix, seen)
			}
			return
		}

		host, port := split(r.Host, map[bool]string{true: "443", false: "80"}[r.Method == http.MethodConnect])
		if !allowed(host) {
			note("REFUSE", host, "not a cluster name or address")
			http.Error(w, "tbx proxy: refused, not a cluster destination", http.StatusForbidden)
			return
		}
		ip, err := resolve(r.Context(), host)
		if err != nil {
			note("NXDOMAIN", host, err.Error())
			http.Error(w, "tbx proxy: "+err.Error(), http.StatusBadGateway)
			return
		}

		if r.Method == http.MethodConnect {
			up, err := net.DialTimeout("tcp", net.JoinHostPort(ip, port), 5*time.Second)
			if err != nil {
				note("DIALFAIL", host, fmt.Sprintf("%s:%s -> %v", ip, port, err))
				http.Error(w, "tbx proxy: "+err.Error(), http.StatusBadGateway)
				return
			}
			hj, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "no hijack", http.StatusInternalServerError)
				return
			}
			cli, _, err := hj.Hijack()
			if err != nil {
				return
			}
			note("CONNECT", host+":"+port, "tunnel -> "+ip)
			_, _ = cli.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
			go func() { _, _ = io.Copy(up, cli); _ = up.Close() }()
			_, _ = io.Copy(cli, up)
			_ = cli.Close()
			return
		}

		// Plain HTTP, absolute-form.
		out := r.Clone(r.Context())
		out.RequestURI = ""
		out.URL.Host = net.JoinHostPort(ip, port)
		tr := &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(ip, port))
		}}
		resp, err := tr.RoundTrip(out)
		if err != nil {
			note("HTTPFAIL", host, fmt.Sprintf("%s:%s -> %v", ip, port, err))
			http.Error(w, "tbx proxy: "+err.Error(), http.StatusBadGateway)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		note("HTTP", host, fmt.Sprintf("%s -> %s", ip, resp.Status))
		for k, v := range resp.Header {
			for _, vv := range v {
				w.Header().Add(k, vv)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	})

	log.Printf("PROTOTYPE proxy listening on %s", *addr)
	log.Printf("  PAC url  http://%s/tbx.pac", *addr)
	log.Printf("  resolver %s   suffix %s", *resolver, *suffix)
	log.Fatal((&http.Server{Addr: *addr, Handler: h}).ListenAndServe())
}
