package rewriter

import (
	"strings"
	"testing"
)

func TestHTMLRewriter_Rewrite(t *testing.T) {
	r := &HTMLRewriter{}
	domain := "example.com"
	proxyBase := ""

	tests := []struct {
		name     string
		input    string
		contains []string
		excludes []string
	}{
		{
			name:     "rewrite href attribute",
			input:    `<a href="/path/page">Link</a>`,
			contains: []string{`href="/example.com/path/page"`},
		},
		{
			name:     "rewrite src attribute",
			input:    `<img src="/images/logo.png">`,
			contains: []string{`src="/example.com/images/logo.png"`},
		},
		{
			name:     "rewrite action attribute",
			input:    `<form action="/submit">`,
			contains: []string{`action="/example.com/submit"`},
		},
		{
			name:     "rewrite data-src attribute",
			input:    `<img data-src="/lazy/image.jpg">`,
			contains: []string{`data-src="/example.com/lazy/image.jpg"`},
		},
		{
			name:     "rewrite absolute URL same domain",
			input:    `<a href="https://example.com/page">Link</a>`,
			contains: []string{`href="/example.com/page"`},
		},
		{
			name:     "rewrite protocol-relative URL",
			input:    `<script src="//cdn.example.com/lib.js"></script>`,
			contains: []string{`src="/cdn.example.com/lib.js"`},
		},
		{
			name:     "preserve data URI",
			input:    `<img src="data:image/png;base64,abc">`,
			contains: []string{`src="data:image/png;base64,abc"`},
		},
		{
			name:     "preserve javascript URI",
			input:    `<a href="javascript:void(0)">Click</a>`,
			contains: []string{`href="javascript:void(0)"`},
		},
		{
			name:     "preserve relative URL",
			input:    `<a href="page.html">Link</a>`,
			contains: []string{`href="page.html"`},
		},
		{
			name:     "preserve hash URL",
			input:    `<a href="#section">Link</a>`,
			contains: []string{`href="#section"`},
		},
		{
			name:     "multiple attributes",
			input:    `<a href="/page" data-href="/other"><img src="/img.png"></a>`,
			contains: []string{`href="/example.com/page"`, `data-href="/example.com/other"`, `src="/example.com/img.png"`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := string(r.Rewrite([]byte(tt.input), domain, proxyBase))

			for _, s := range tt.contains {
				if !strings.Contains(result, s) {
					t.Errorf("Expected result to contain %q, got: %s", s, result)
				}
			}

			for _, s := range tt.excludes {
				if strings.Contains(result, s) {
					t.Errorf("Expected result to NOT contain %q, got: %s", s, result)
				}
			}
		})
	}
}

func TestHTMLRewriter_Srcset(t *testing.T) {
	r := &HTMLRewriter{}
	domain := "example.com"
	proxyBase := ""

	tests := []struct {
		name     string
		input    string
		contains []string
	}{
		{
			name:     "srcset with sizes",
			input:    `<img srcset="/small.jpg 1x, /large.jpg 2x">`,
			contains: []string{"/example.com/small.jpg 1x", "/example.com/large.jpg 2x"},
		},
		{
			name:     "srcset with widths",
			input:    `<img srcset="/img-320.jpg 320w, /img-640.jpg 640w">`,
			contains: []string{"/example.com/img-320.jpg 320w", "/example.com/img-640.jpg 640w"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := string(r.Rewrite([]byte(tt.input), domain, proxyBase))

			for _, s := range tt.contains {
				if !strings.Contains(result, s) {
					t.Errorf("Expected result to contain %q, got: %s", s, result)
				}
			}
		})
	}
}

func TestHTMLRewriter_MetaRefresh(t *testing.T) {
	r := &HTMLRewriter{}
	domain := "example.com"
	proxyBase := ""

	input := `<meta http-equiv="refresh" content="5; url=/new-page">`
	result := string(r.Rewrite([]byte(input), domain, proxyBase))

	if !strings.Contains(result, "/example.com/new-page") {
		t.Errorf("Expected meta refresh URL to be rewritten, got: %s", result)
	}
}

func TestHTMLRewriter_InlineStyle(t *testing.T) {
	r := &HTMLRewriter{}
	domain := "example.com"
	proxyBase := ""

	input := `<div style="background: url(/images/bg.png)"></div>`
	result := string(r.Rewrite([]byte(input), domain, proxyBase))

	if !strings.Contains(result, "/example.com/images/bg.png") {
		t.Errorf("Expected inline style URL to be rewritten, got: %s", result)
	}
}

func TestHTMLRewriter_BaseTag(t *testing.T) {
	r := &HTMLRewriter{}
	domain := "example.com"
	proxyBase := ""

	input := `<base href="https://example.com/">`
	result := string(r.Rewrite([]byte(input), domain, proxyBase))

	if !strings.Contains(result, `<base href="/example.com/"`) {
		t.Errorf("Expected base tag to be rewritten, got: %s", result)
	}
}
