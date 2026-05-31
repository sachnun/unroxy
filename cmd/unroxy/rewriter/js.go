package rewriter

import (
	"regexp"
)

type JSRewriter struct{}

var (
	jsStaticImportDoubleQuote   = regexp.MustCompile(`(import\s+(?:[^'"]+\s+from\s+)?)("/[^"]+")`)
	jsStaticImportSingleQuote   = regexp.MustCompile(`(import\s+(?:[^'"]+\s+from\s+)?)('/[^']+')`)
	jsDynamicImportDoubleQuote  = regexp.MustCompile(`(import\s*\(\s*)("/[^"]+")(\s*\))`)
	jsDynamicImportSingleQuote  = regexp.MustCompile(`(import\s*\(\s*)('/[^']+')(\s*\))`)
	jsFetchDoubleQuote          = regexp.MustCompile(`(fetch\s*\(\s*)("/[^"]+")`)
	jsFetchSingleQuote          = regexp.MustCompile(`(fetch\s*\(\s*)('/[^']+')`)
	jsURLConstructorDoubleQuote = regexp.MustCompile(`(new\s+URL\s*\(\s*)("/[^"]+")`)
	jsURLConstructorSingleQuote = regexp.MustCompile(`(new\s+URL\s*\(\s*)('/[^']+')`)

	jsWorkerDoubleQuote          = regexp.MustCompile(`((?:new\s+)?Worker\s*\(\s*)("/[^"]+")`)
	jsWorkerSingleQuote          = regexp.MustCompile(`((?:new\s+)?Worker\s*\(\s*)('/[^']+')`)
	jsEventSourceDoubleQuote     = regexp.MustCompile(`((?:new\s+)?EventSource\s*\(\s*)("/[^"]+")`)
	jsEventSourceSingleQuote     = regexp.MustCompile(`((?:new\s+)?EventSource\s*\(\s*)('/[^']+')`)
	jsSWRegisterDoubleQuote      = regexp.MustCompile(`(\.register\s*\(\s*)("/[^"]+")`)
	jsSWRegisterSingleQuote      = regexp.MustCompile(`(\.register\s*\(\s*)('/[^']+')`)
	jsXHROpenDoubleQuote         = regexp.MustCompile(`(\.open\s*\(\s*"[^"]*"\s*,\s*)("/[^"]+")`)
	jsXHROpenSingleQuote         = regexp.MustCompile(`(\.open\s*\(\s*'[^']*'\s*,\s*)('/[^']+')`)
	jsWindowOpenDoubleQuote      = regexp.MustCompile(`(window\.open\s*\(\s*)("/[^"]+")`)
	jsWindowOpenSingleQuote      = regexp.MustCompile(`(window\.open\s*\(\s*)('/[^']+')`)
	jsLocationHrefDoubleQuote    = regexp.MustCompile(`(location\.href\s*=\s*)("/[^"]+")`)
	jsLocationHrefSingleQuote    = regexp.MustCompile(`(location\.href\s*=\s*)('/[^']+')`)
	jsLocationAssignDoubleQuote  = regexp.MustCompile(`(location\.(?:assign|replace)\s*\(\s*)("/[^"]+")`)
	jsLocationAssignSingleQuote  = regexp.MustCompile(`(location\.(?:assign|replace)\s*\(\s*)('/[^']+')`)
	jsHistoryPushDoubleQuote     = regexp.MustCompile(`(history\.(?:pushState|replaceState)\s*\([^,]+,\s*[^,]+,\s*)("/[^"]+")`)
	jsHistoryPushSingleQuote     = regexp.MustCompile(`(history\.(?:pushState|replaceState)\s*\([^,]+,\s*[^,]+,\s*)('/[^']+')`)
	jsSendBeaconDoubleQuote      = regexp.MustCompile(`(sendBeacon\s*\(\s*)("/[^"]+")`)
	jsSendBeaconSingleQuote      = regexp.MustCompile(`(sendBeacon\s*\(\s*)('/[^']+')`)
	jsImportScriptsDoubleQuote   = regexp.MustCompile(`(importScripts\s*\(\s*)("/[^"]+")`)
	jsImportScriptsSingleQuote   = regexp.MustCompile(`(importScripts\s*\(\s*)('/[^']+')`)
	jsDocWriteScriptDoubleQuote  = regexp.MustCompile(`(document\.(?:write|writeln)\s*\(\s*"<[^>]*src\s*=\s*")(/[^"]+)(")`)
	jsDocWriteScriptSingleQuote  = regexp.MustCompile(`(document\.(?:write|writeln)\s*\(\s*'<[^>]*src\s*=\s*')(/[^']+)(')`)
)

func (r *JSRewriter) SupportedContentType() string {
	return "application/javascript"
}

func (r *JSRewriter) Rewrite(body []byte, domain, proxyBase string) []byte {
	js := string(body)

	rewriteQuotedURL := func(quotedURL string) string {
		quote := quotedURL[0:1]
		url := quotedURL[1 : len(quotedURL)-1]
		newURL := ToProxyURL(url, domain, proxyBase)
		return quote + newURL + quote
	}

	replaceFunc := func(patterns ...*regexp.Regexp) {
		for _, p := range patterns {
			js = p.ReplaceAllStringFunc(js, func(match string) string {
				parts := p.FindStringSubmatch(match)
				if len(parts) < 3 {
					return match
				}
				return parts[1] + rewriteQuotedURL(parts[2])
			})
		}
	}

	replaceFunc(
		jsStaticImportDoubleQuote, jsStaticImportSingleQuote,
		jsFetchDoubleQuote, jsFetchSingleQuote,
		jsWorkerDoubleQuote, jsWorkerSingleQuote,
		jsEventSourceDoubleQuote, jsEventSourceSingleQuote,
		jsSWRegisterDoubleQuote, jsSWRegisterSingleQuote,
		jsXHROpenDoubleQuote, jsXHROpenSingleQuote,
		jsWindowOpenDoubleQuote, jsWindowOpenSingleQuote,
		jsLocationHrefDoubleQuote, jsLocationHrefSingleQuote,
		jsLocationAssignDoubleQuote, jsLocationAssignSingleQuote,
		jsSendBeaconDoubleQuote, jsSendBeaconSingleQuote,
		jsImportScriptsDoubleQuote, jsImportScriptsSingleQuote,
		jsURLConstructorDoubleQuote, jsURLConstructorSingleQuote,
		jsHistoryPushDoubleQuote, jsHistoryPushSingleQuote,
	)

	for _, p := range []*regexp.Regexp{
		jsDynamicImportDoubleQuote, jsDynamicImportSingleQuote,
	} {
		js = p.ReplaceAllStringFunc(js, func(match string) string {
			parts := p.FindStringSubmatch(match)
			if len(parts) != 4 {
				return match
			}
			return parts[1] + rewriteQuotedURL(parts[2]) + parts[3]
		})
	}

	for _, p := range []*regexp.Regexp{
		jsDocWriteScriptDoubleQuote, jsDocWriteScriptSingleQuote,
	} {
		js = p.ReplaceAllStringFunc(js, func(match string) string {
			parts := p.FindStringSubmatch(match)
			if len(parts) != 4 {
				return match
			}
			return parts[1] + ToProxyURL(parts[2], domain, proxyBase) + parts[3]
		})
	}

	return []byte(js)
}
