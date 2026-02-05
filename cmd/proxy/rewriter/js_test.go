package rewriter

import (
	"strings"
	"testing"
)

func TestJSRewriter_Rewrite(t *testing.T) {
	r := &JSRewriter{}
	domain := "example.com"
	proxyBase := ""

	tests := []struct {
		name     string
		input    string
		contains []string
	}{
		{
			name:     "rewrite static import with from",
			input:    `import { foo } from '/modules/foo.js';`,
			contains: []string{`from '/example.com/modules/foo.js'`},
		},
		{
			name:     "rewrite static import double quotes",
			input:    `import { foo } from "/modules/foo.js";`,
			contains: []string{`from "/example.com/modules/foo.js"`},
		},
		{
			name:     "rewrite dynamic import",
			input:    `const mod = import('/modules/lazy.js');`,
			contains: []string{`import('/example.com/modules/lazy.js')`},
		},
		{
			name:     "rewrite fetch with root path",
			input:    `fetch('/api/data')`,
			contains: []string{`fetch('/example.com/api/data'`},
		},
		{
			name:     "rewrite new URL constructor",
			input:    `new URL('/path/file', base)`,
			contains: []string{`new URL('/example.com/path/file'`},
		},
		{
			name:     "preserve relative import",
			input:    `import { foo } from './foo.js';`,
			contains: []string{`from './foo.js'`},
		},
		{
			name:     "preserve external URL fetch",
			input:    `fetch('https://api.example.com/data')`,
			contains: []string{`fetch('https://api.example.com/data')`},
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

func TestJSRewriter_SupportedContentType(t *testing.T) {
	r := &JSRewriter{}
	if r.SupportedContentType() != "application/javascript" {
		t.Errorf("Expected application/javascript, got %s", r.SupportedContentType())
	}
}

func TestJSRewriter_ComplexScript(t *testing.T) {
	r := &JSRewriter{}
	domain := "example.com"
	proxyBase := ""

	input := `
import { Component } from '/lib/framework.js';
import '/styles/main.css';

async function loadModule() {
    const mod = await import('/modules/dynamic.js');
    const data = await fetch('/api/users');
    return data.json();
}
`

	result := string(r.Rewrite([]byte(input), domain, proxyBase))

	expected := []string{
		"/example.com/lib/framework.js",
		"/example.com/styles/main.css",
		"/example.com/modules/dynamic.js",
		"/example.com/api/users",
	}

	for _, s := range expected {
		if !strings.Contains(result, s) {
			t.Errorf("Expected result to contain %q", s)
		}
	}
}
