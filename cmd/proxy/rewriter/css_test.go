package rewriter

import (
	"strings"
	"testing"
)

func TestCSSRewriter_Rewrite(t *testing.T) {
	r := &CSSRewriter{}
	domain := "example.com"
	proxyBase := ""

	tests := []struct {
		name     string
		input    string
		contains []string
		excludes []string
	}{
		{
			name:     "rewrite url() with root path",
			input:    `background: url(/images/bg.png);`,
			contains: []string{`url("/example.com/images/bg.png")`},
		},
		{
			name:     "rewrite url() with quotes",
			input:    `background: url("/images/bg.png");`,
			contains: []string{`url("/example.com/images/bg.png")`},
		},
		{
			name:     "rewrite url() with single quotes",
			input:    `background: url('/images/bg.png');`,
			contains: []string{`url("/example.com/images/bg.png")`},
		},
		{
			name:     "preserve data URI in url()",
			input:    `background: url(data:image/png;base64,abc);`,
			contains: []string{`url(data:image/png;base64,abc)`},
		},
		{
			name:     "rewrite multiple url()",
			input:    `background: url(/img1.png), url(/img2.png);`,
			contains: []string{`url("/example.com/img1.png")`, `url("/example.com/img2.png")`},
		},
		{
			name:     "rewrite @import with url()",
			input:    `@import url("/styles/main.css");`,
			contains: []string{`@import url("/example.com/styles/main.css")`},
		},
		{
			name:     "rewrite @import without url()",
			input:    `@import "/styles/main.css";`,
			contains: []string{`@import "/example.com/styles/main.css"`},
		},
		{
			name:     "rewrite font-face src",
			input:    `src: url(/fonts/font.woff2) format('woff2');`,
			contains: []string{`url("/example.com/fonts/font.woff2")`},
		},
		{
			name:     "preserve relative url()",
			input:    `background: url(../images/bg.png);`,
			contains: []string{`url("../images/bg.png")`},
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

func TestCSSRewriter_SupportedContentType(t *testing.T) {
	r := &CSSRewriter{}
	if r.SupportedContentType() != "text/css" {
		t.Errorf("Expected text/css, got %s", r.SupportedContentType())
	}
}
