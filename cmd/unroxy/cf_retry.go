package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"

	solver "unroxy/cmd/unroxy/cfjsd"
)

type CFRetryTransport struct {
	base   http.RoundTripper
	logger *log.Logger
}

type cfChallengeParams struct {
	R string
	T string
}

func NewCFRetryTransport(base http.RoundTripper, logger *log.Logger) *CFRetryTransport {
	return &CFRetryTransport{base: base, logger: logger}
}

var cfParamRe = regexp.MustCompile(`__CF\$cv\$params\s*=\s*\{[^}]+\}`)
var cfRRe = regexp.MustCompile(`\br\s*:\s*['"]([a-fA-F0-9]+)['"]`)
var cfTRe = regexp.MustCompile(`\bt\s*:\s*['"]([A-Za-z0-9+/=]+)['"]`)

func (t *CFRetryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return resp, err
	}

	if resp.StatusCode/100 == 2 {
		return resp, nil
	}

	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("cf read body: %w", err)
	}

	if !cfParamRe.Match(body) {
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return resp, nil
	}

	params := extractCFParams(body)
	if params == nil {
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return resp, nil
	}

	targetURL := fmt.Sprintf("%s://%s%s", req.URL.Scheme, req.URL.Host, req.URL.Path)
	if t.logger != nil {
		t.logger.Printf("[CF] challenge detected for %s", targetURL)
	}

	cfClearance, err := t.solveChallenge(targetURL, params)
	if err != nil {
		if t.logger != nil {
			t.logger.Printf("[CF] solve failed: %v", err)
		}
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return resp, nil
	}

	if t.logger != nil {
		t.logger.Printf("[CF] solved, retrying with cf_clearance=%s", truncate(cfClearance, 40))
	}

	retryReq := req.Clone(req.Context())
	retryReq.AddCookie(&http.Cookie{
		Name:  "cf_clearance",
		Value: cfClearance,
	})

	return t.base.RoundTrip(retryReq)
}

func extractCFParams(body []byte) *cfChallengeParams {
	block := cfParamRe.Find(body)
	if block == nil {
		return nil
	}

	params := &cfChallengeParams{}

	if m := cfRRe.FindSubmatch(block); len(m) > 1 {
		params.R = string(m[1])
	}
	if m := cfTRe.FindSubmatch(block); len(m) > 1 {
		params.T = string(m[1])
	}

	if params.R == "" || params.T == "" {
		return nil
	}

	return params
}

func (t *CFRetryTransport) solveChallenge(targetURL string, params *cfChallengeParams) (string, error) {
	s, err := solver.NewSolver(targetURL, false)
	if err != nil {
		return "", fmt.Errorf("jsd init: %w", err)
	}

	result, err := s.SolveFromData(solver.SolveData{
		R: params.R,
		T: params.T,
	})
	if err != nil {
		return "", fmt.Errorf("jsd solve: %w", err)
	}

	if result.CfClearance != "" {
		return result.CfClearance, nil
	}

	return "", fmt.Errorf("no cf_clearance (status=%d): %s", result.StatusCode, truncate(result.Body, 200))
}

func truncate(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", "")
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}
