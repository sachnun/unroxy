package rewriter

import (
	"net/http"
	"strings"
)

func RewriteHeaders(resp *http.Response, domain, proxyBase string) {
	if location := resp.Header.Get("Location"); location != "" {
		newLocation := ToProxyURL(location, domain, proxyBase)
		resp.Header.Set("Location", newLocation)
	}

	if contentLocation := resp.Header.Get("Content-Location"); contentLocation != "" {
		newContentLocation := ToProxyURL(contentLocation, domain, proxyBase)
		resp.Header.Set("Content-Location", newContentLocation)
	}

	if link := resp.Header.Get("Link"); link != "" {
		newLink := rewriteLinkHeader(link, domain, proxyBase)
		resp.Header.Set("Link", newLink)
	}

	cookies := resp.Header.Values("Set-Cookie")
	if len(cookies) > 0 {
		resp.Header.Del("Set-Cookie")
		for _, cookie := range cookies {
			newCookie := rewriteCookiePath(cookie, domain, proxyBase)
			resp.Header.Add("Set-Cookie", newCookie)
		}
	}

	resp.Header.Del("Content-Security-Policy")
	resp.Header.Del("Content-Security-Policy-Report-Only")
	resp.Header.Del("X-Frame-Options")

	resp.Header.Set("Access-Control-Allow-Origin", "*")
}

func rewriteLinkHeader(link, domain, proxyBase string) string {
	parts := strings.Split(link, ",")
	var result []string

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		start := strings.Index(part, "<")
		end := strings.Index(part, ">")
		if start == -1 || end == -1 || end <= start {
			result = append(result, part)
			continue
		}

		url := part[start+1 : end]
		newURL := ToProxyURL(url, domain, proxyBase)
		newPart := part[:start+1] + newURL + part[end:]
		result = append(result, newPart)
	}

	return strings.Join(result, ", ")
}

func rewriteCookiePath(cookie, domain, proxyBase string) string {
	lowerCookie := strings.ToLower(cookie)
	pathIdx := strings.Index(lowerCookie, "path=")

	if pathIdx == -1 {
		return cookie + "; Path=" + proxyBase + "/" + domain + "/"
	}

	valueStart := pathIdx + 5
	valueEnd := valueStart

	for valueEnd < len(cookie) && cookie[valueEnd] != ';' {
		valueEnd++
	}

	oldPath := strings.TrimSpace(cookie[valueStart:valueEnd])
	newPath := proxyBase + "/" + domain + oldPath

	return cookie[:valueStart] + newPath + cookie[valueEnd:]
}

func RewriteRequestHeaders(req *http.Request, domain string) {
	req.Host = domain

	req.Header.Set("Accept-Encoding", "identity")

	req.Header["X-Forwarded-For"] = nil
	req.Header.Del("X-Real-IP")
	req.Header.Del("X-Originating-IP")
	req.Header.Del("True-Client-IP")
	req.Header.Del("Client-IP")
	req.Header.Del("Forwarded")
	req.Header.Del("X-Forwarded-Host")
	req.Header.Del("X-Forwarded-Proto")
	req.Header.Del("CF-Connecting-IP")
}
