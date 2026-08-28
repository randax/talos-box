package mirror

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	challengeRe = regexp.MustCompile(`([A-Za-z0-9_-]+)="([^"]*)"`)
	blobScopeRe = regexp.MustCompile(`^/v2/(.+)/blobs/`)
)

type bearerChallenge struct {
	realm   string
	service string
	scope   string
}

func bearerChallengeFrom(headers []string) (bearerChallenge, bool) {
	for _, header := range headers {
		for _, single := range splitChallenges(header) {
			if challenge, ok := parseBearerChallenge(single); ok {
				return challenge, true
			}
		}
	}
	return bearerChallenge{}, false
}

func splitChallenges(header string) []string {
	var segments []string
	quoted := false
	start := 0
	for i := 0; i < len(header); i++ {
		switch header[i] {
		case '"':
			quoted = !quoted
		case '\\':
			if quoted {
				i++
			}
		case ',':
			if !quoted {
				segments = append(segments, header[start:i])
				start = i + 1
			}
		}
	}
	segments = append(segments, header[start:])

	var challenges []string
	current := ""
	for _, segment := range segments {
		segment = strings.TrimSpace(segment)
		if segment == "" {
			continue
		}
		head, _, _ := strings.Cut(segment, "=")
		if current != "" && !strings.ContainsAny(strings.TrimSpace(head), " \t") && strings.Contains(segment, "=") {
			current += ", " + segment
			continue
		}
		if current != "" {
			challenges = append(challenges, current)
		}
		current = segment
	}
	if current != "" {
		challenges = append(challenges, current)
	}
	return challenges
}

func parseBearerChallenge(header string) (bearerChallenge, bool) {
	scheme, rest, ok := strings.Cut(strings.TrimSpace(header), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return bearerChallenge{}, false
	}
	params := map[string]string{}
	for _, match := range challengeRe.FindAllStringSubmatch(rest, -1) {
		params[strings.ToLower(match[1])] = match[2]
	}
	return bearerChallenge{realm: params["realm"], service: params["service"], scope: params["scope"]}, true
}

const defaultTokenLifetime = 60 * time.Second

func (s *Server) negotiateToken(ctx context.Context, challenge bearerChallenge, requestScope string, policy requestPolicy) (string, error) {
	if challenge.realm == "" {
		return "", fmt.Errorf("auth challenge without realm")
	}
	scope := challenge.scope
	if scope == "" {
		scope = requestScope
	}
	realm, err := url.Parse(challenge.realm)
	if err != nil {
		return "", fmt.Errorf("auth challenge realm %q: %w", challenge.realm, err)
	}
	query := realm.Query()
	if challenge.service != "" {
		query.Set("service", challenge.service)
	}
	if scope != "" {
		query.Set("scope", scope)
	}
	realm.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return "", err
	}
	resp, err := s.doPlainRequest(request, policy)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint %s returned %d", realm.Redacted(), resp.StatusCode)
	}
	var payload struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := decodeJSON(resp.Body, &payload); err != nil {
		return "", fmt.Errorf("token response: %w", err)
	}
	bearer := payload.Token
	if bearer == "" {
		bearer = payload.AccessToken
	}
	if bearer == "" {
		return "", fmt.Errorf("token endpoint returned no token")
	}
	if requestScope != "" {
		s.mu.Lock()
		if s.tokens == nil {
			s.tokens = make(map[string]token)
		}
		now := s.currentTime()
		for key, entry := range s.tokens {
			if !now.Before(entry.expires) {
				delete(s.tokens, key)
			}
		}
		s.tokens[requestScope] = token{value: bearer, expires: now.Add(tokenLifetime(payload.ExpiresIn))}
		s.mu.Unlock()
	}
	return bearer, nil
}

func tokenLifetime(expiresIn int) time.Duration {
	lifetime := defaultTokenLifetime
	if expiresIn > 0 {
		lifetime = time.Duration(expiresIn) * time.Second
	}
	skew := lifetime / 10
	if skew > 30*time.Second {
		skew = 30 * time.Second
	}
	return lifetime - skew
}

func (s *Server) currentTime() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *Server) cachedToken(scope string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.tokens[scope]
	if !ok || !s.currentTime().Before(entry.expires) {
		return ""
	}
	return entry.value
}

func (s *Server) forgetToken(scope string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tokens, scope)
}

func scopeOf(requestPath string) string {
	if match := manifestPathRe.FindStringSubmatch(requestPath); match != nil {
		return "repository:" + match[1] + ":pull"
	}
	if match := blobScopeRe.FindStringSubmatch(requestPath); match != nil {
		return "repository:" + match[1] + ":pull"
	}
	return ""
}

type retryPolicy struct {
	MaxAttempts int
	MaxDelay    time.Duration
	Sleep       func(context.Context, time.Duration) error
	Now         func() time.Time
}

type requestPolicy struct {
	Retry retryPolicy
}

func defaultRequestPolicy() requestPolicy {
	// A fill, including containerd's initial HEAD probe, makes at most three
	// attempts. There are at most two retry sleeps, each capped at 30 seconds,
	// so rate limiting can add no more than 60 seconds of retry delay (network
	// request time is bounded separately by the HTTP client).
	return requestPolicy{Retry: retryPolicy{
		MaxAttempts: 3,
		MaxDelay:    30 * time.Second,
		Sleep:       sleepContext,
		Now:         time.Now,
	}}
}

func (s *Server) fillRequestPolicy() requestPolicy {
	policy := defaultRequestPolicy()
	if s.retrySleep != nil {
		policy.Retry.Sleep = s.retrySleep
	}
	return policy
}

func immediateRequestPolicy() requestPolicy {
	policy := defaultRequestPolicy()
	policy.Retry.MaxAttempts = 1
	return policy
}

func normalizedRetryPolicy(policy retryPolicy) retryPolicy {
	if policy.MaxAttempts <= 0 {
		policy.MaxAttempts = 1
	}
	if policy.MaxDelay <= 0 {
		policy.MaxDelay = 30 * time.Second
	}
	if policy.Sleep == nil {
		policy.Sleep = sleepContext
	}
	if policy.Now == nil {
		policy.Now = time.Now
	}
	return policy
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func parseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second, true
	}
	when, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	delay := when.Sub(now)
	if delay < 0 {
		delay = 0
	}
	return delay, true
}

func (policy retryPolicy) retryDelay(header string, retry int) time.Duration {
	if delay, ok := parseRetryAfter(header, policy.Now()); ok {
		if delay > policy.MaxDelay {
			return policy.MaxDelay
		}
		return delay
	}
	delay := 500 * time.Millisecond
	if retry > 0 {
		delay = time.Second
	}
	if delay > policy.MaxDelay {
		return policy.MaxDelay
	}
	return delay
}

func closeRetryResponse(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

// doRegistryRequest performs generic Bearer negotiation and bounded 429
// retries. All registry-specific values come from the advertised challenge.
func (s *Server) doRegistryRequest(request *http.Request, policy requestPolicy) (*http.Response, error) {
	retry := normalizedRetryPolicy(policy.Retry)
	for attempt := 0; attempt < retry.MaxAttempts; attempt++ {
		resp, err := s.doRegistryAttempt(request, policy)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusTooManyRequests || attempt+1 == retry.MaxAttempts {
			return resp, nil
		}
		delay := retry.retryDelay(resp.Header.Get("Retry-After"), attempt)
		closeRetryResponse(resp)
		if err := retry.Sleep(request.Context(), delay); err != nil {
			return nil, err
		}
	}
	panic("unreachable")
}

func (s *Server) doRegistryAttempt(request *http.Request, policy requestPolicy) (*http.Response, error) {
	scope := scopeOf(request.URL.Path)
	initial := request.Clone(request.Context())
	if bearer := s.cachedToken(scope); bearer != "" {
		initial.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := s.client.Do(initial)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}
	challenge, ok := bearerChallengeFrom(resp.Header.Values("WWW-Authenticate"))
	if !ok {
		return resp, nil
	}
	closeRetryResponse(resp)
	s.forgetToken(scope)
	bearer, err := s.negotiateToken(request.Context(), challenge, scope, policy)
	if err != nil {
		return nil, err
	}
	authorized := request.Clone(request.Context())
	authorized.Header.Set("Authorization", "Bearer "+bearer)
	return s.client.Do(authorized)
}

func (s *Server) doPlainRequest(request *http.Request, policy requestPolicy) (*http.Response, error) {
	retry := normalizedRetryPolicy(policy.Retry)
	for attempt := 0; attempt < retry.MaxAttempts; attempt++ {
		resp, err := s.client.Do(request.Clone(request.Context()))
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusTooManyRequests || attempt+1 == retry.MaxAttempts {
			return resp, nil
		}
		delay := retry.retryDelay(resp.Header.Get("Retry-After"), attempt)
		closeRetryResponse(resp)
		if err := retry.Sleep(request.Context(), delay); err != nil {
			return nil, err
		}
	}
	panic("unreachable")
}

// fetch prepares a guest request for the configured upstream. The caller
// chooses whether bounded retries are appropriate for this request path.
func (s *Server) fetch(r *http.Request, policy requestPolicy) (*http.Response, error) {
	request, err := http.NewRequestWithContext(r.Context(), r.Method, s.base+r.URL.RequestURI(), nil)
	if err != nil {
		return nil, err
	}
	for _, header := range []string{"Accept", "Range"} {
		if value := r.Header.Get(header); value != "" {
			request.Header.Set(header, value)
		}
	}
	return s.doRegistryRequest(request, policy)
}
