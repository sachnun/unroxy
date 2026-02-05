package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/http/httputil"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	regexPool = sync.Pool{
		New: func() any { return &regexCache{} },
	}

	userAgents = []string{
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:121.0) Gecko/20100101 Firefox/121.0",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.2 Safari/605.1.15",
		"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36 Edg/120.0.0.0",
	}
)

type regexCache struct {
	absoluteURL *regexp.Regexp
	attrURL     *regexp.Regexp
	cssURL      *regexp.Regexp
}

func getRegexCache(domain string) *regexCache {
	cache := regexPool.Get().(*regexCache)
	if cache.absoluteURL == nil {
		cache.absoluteURL = regexp.MustCompile(`https?://` + regexp.QuoteMeta(domain) + `([^\s"'<>]*)`)
		cache.attrURL = regexp.MustCompile(`(src|href|action|srcset|data-src|data-href)="([^"]*)"`)
		cache.cssURL = regexp.MustCompile(`url\(([^)]*)\)`)
	}
	return cache
}

func putRegexCache(cache *regexCache) { regexPool.Put(cache) }

func generateRandomIP() string {
	for {
		a := rand.Intn(224)
		if a == 0 || a == 10 || a == 127 {
			continue
		}
		b := rand.Intn(256)
		if a == 172 && b >= 16 && b <= 31 {
			continue
		}
		if a == 192 && b == 168 {
			continue
		}
		return fmt.Sprintf("%d.%d.%d.%d", a, b, rand.Intn(256), rand.Intn(256))
	}
}

func init() { rand.Seed(time.Now().UnixNano()) }

func main() {
	log.Println("Server running on :8080")
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
		if len(parts) < 1 {
			http.Error(w, "Invalid path", 400)
			return
		}
		domain := parts[0]
		remainingPath := "/" + strings.Join(parts[1:], "/")

		proxy := &httputil.ReverseProxy{
			Director: func(req *http.Request) {
				req.URL.Scheme = "https"
				req.URL.Host = domain
				req.URL.Path = remainingPath
				req.Host = domain

				randomIP := generateRandomIP()
				req.Header.Set("X-Forwarded-For", randomIP)
				req.Header.Set("X-Real-IP", randomIP)
				req.Header.Set("X-Originating-IP", randomIP)
				req.Header.Set("True-Client-IP", randomIP)
				req.Header.Set("Client-IP", randomIP)
				req.Header.Set("User-Agent", userAgents[rand.Intn(len(userAgents))])
			},
			ModifyResponse: func(resp *http.Response) error {
				if !strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
					return nil
				}
				body, err := io.ReadAll(resp.Body)
				if err != nil {
					return err
				}
				resp.Body.Close()

				html := string(body)
				cache := getRegexCache(domain)
				defer putRegexCache(cache)

				html = cache.absoluteURL.ReplaceAllString(html, "/"+domain+"$1")
				html = cache.attrURL.ReplaceAllStringFunc(html, func(match string) string {
					m := cache.attrURL.FindStringSubmatch(match)
					if len(m) == 3 && strings.HasPrefix(m[2], "/") && !strings.HasPrefix(m[2], "//") && !strings.HasPrefix(m[2], "/"+domain) {
						return m[1] + `="/` + domain + m[2] + `"`
					}
					return match
				})
				html = cache.cssURL.ReplaceAllStringFunc(html, func(match string) string {
					m := cache.cssURL.FindStringSubmatch(match)
					if len(m) == 2 {
						url := strings.Trim(m[1], `'"`)
						if strings.HasPrefix(url, "/") && !strings.HasPrefix(url, "//") && !strings.HasPrefix(url, "/"+domain) {
							return `url("/` + domain + url + `")`
						}
					}
					return match
				})

				resp.Body = io.NopCloser(bytes.NewBufferString(html))
				resp.Header.Set("Content-Length", strconv.Itoa(len(html)))
				return nil
			},
		}
		proxy.ServeHTTP(w, r)
	})
	http.ListenAndServe(":8080", nil)
}
