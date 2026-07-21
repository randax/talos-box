package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

const factoryDoctorURL = "https://factory.talos.dev"

type httpDo func(*http.Request) (*http.Response, error)

type egressErrorKind int

const (
	egressUnknownAuthority egressErrorKind = iota
	egressTimeout
	egressConnectionReset
	egressOther
)

func newDoctorHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	return &http.Client{
		Transport: transport,
		Timeout:   5 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func probeFactoryEgress(do httpDo) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var handshakeComplete atomic.Bool
	trace := &httptrace.ClientTrace{
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			if err == nil {
				handshakeComplete.Store(true)
			}
		},
	}
	ctx = httptrace.WithClientTrace(ctx, trace)
	request, err := http.NewRequestWithContext(ctx, http.MethodHead, factoryDoctorURL, nil)
	if err != nil {
		return err
	}
	response, err := do(request)
	if err != nil {
		// Response status and post-handshake HTTP failures are irrelevant to this
		// probe; it is specifically testing whether the TLS handshake can complete.
		if handshakeComplete.Load() {
			return nil
		}
		return err
	}
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	return nil
}

func egressFinding(err error) doctorFinding {
	if err == nil {
		return doctorFinding{level: "PASS", check: "egress"}
	}
	finding := doctorFinding{level: "WARN", check: "egress"}
	switch classifyEgressError(err) {
	case egressUnknownAuthority:
		finding.detail = "TLS interception certificate is signed by an unknown authority; install the trusted corporate CA in the System keychain"
	case egressTimeout:
		finding.detail = "connection timed out (likely proxy-only egress); HTTPS_PROXY must be set in the shell that starts tbx"
	case egressConnectionReset:
		finding.detail = "connection reset during the TLS handshake (TLS filtered)"
	default:
		finding.detail = fmt.Sprintf("connection failed: %v", err)
	}
	return finding
}

func classifyEgressError(err error) egressErrorKind {
	var unknownAuthority x509.UnknownAuthorityError
	if errors.As(err, &unknownAuthority) || strings.Contains(strings.ToLower(err.Error()), "unknown authority") {
		return egressUnknownAuthority
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return egressTimeout
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return egressTimeout
	}
	if errors.Is(err, syscall.ECONNRESET) || strings.Contains(strings.ToLower(err.Error()), "connection reset") {
		return egressConnectionReset
	}
	return egressOther
}
