package validator

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

func client(url string) *JEV {
	j := NewJEV("jev-1.13.0")
	j.Endpoint, j.Key, j.Sleep = url, "test-key", func(time.Duration) {}
	return j
}

func req(content string) Request {
	return Request{
		State: State{Path: "a.java", Language: "java", Content: content},
		Questions: map[string]Question{
			"r1": {Type: "noul", Instructions: "x"},
			"r2": {Type: "choice", Instructions: "y", Criteria: map[string]string{"compliant": "", "bad": ""}},
		},
	}
}

func TestJEVSuccessAndCompression(t *testing.T) {
	var gotEncoding string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("auth header %q", r.Header.Get("Authorization"))
		}
		gotEncoding = r.Header.Get("Content-Encoding")
		var body io.Reader = r.Body
		if gotEncoding == "zstd" {
			d, _ := zstd.NewReader(r.Body)
			body = d
		}
		var in map[string]any
		if err := json.NewDecoder(body).Decode(&in); err != nil {
			t.Errorf("decode: %v", err)
		}
		if in["model"] != "jev-1.13.0" {
			t.Errorf("model %v", in["model"])
		}
		io.WriteString(w, `{"model":"jev-1.13.0","answers":{"r1":{"type":"noul","noul":0.9},"r2":{"type":"choice","choice":"bad","probabilities":{"compliant":0.25,"bad":0.75},"confidence":0.5}},"usage":{"input_tokens":123,"output_tokens":1}}`)
	}))
	defer srv.Close()

	res, err := client(srv.URL).Validate(context.Background(), req(strings.Repeat("x", 2000)))
	if err != nil {
		t.Fatal(err)
	}
	if gotEncoding != "zstd" {
		t.Errorf("body ≥ 1 KiB should be zstd, got %q", gotEncoding)
	}
	if res.Probabilities["r1"] != 0.9 || res.Probabilities["r2"] != 0.75 || res.InputTokens != 123 {
		t.Errorf("result %+v", res)
	}
	if _, err := client(srv.URL).Validate(context.Background(), req("short")); err != nil || gotEncoding != "" {
		t.Errorf("small body should be uncompressed: %q %v", gotEncoding, err)
	}
}

func TestJEVErrors(t *testing.T) {
	var status atomic.Int32
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		s := int(status.Load())
		if s == 429 && n >= 3 { // recovers on the 3rd try
			io.WriteString(w, `{"model":"m","answers":{"r1":{"type":"noul","noul":0.1},"r2":{"type":"choice","probabilities":{"compliant":1,"bad":0}}},"usage":{"input_tokens":1}}`)
			return
		}
		w.WriteHeader(s)
		if s == 400 {
			io.WriteString(w, `{"error":"max_tokens_exceeded"}`)
		}
	}))
	defer srv.Close()

	status.Store(401)
	if _, err := client(srv.URL).Validate(context.Background(), req("a")); !errors.Is(err, ErrAuth) {
		t.Errorf("401: %v", err)
	}
	status.Store(400)
	if _, err := client(srv.URL).Validate(context.Background(), req("a")); !errors.Is(err, ErrTooLarge) {
		t.Errorf("400 max_tokens: %v", err)
	}
	status.Store(429)
	calls.Store(0)
	if _, err := client(srv.URL).Validate(context.Background(), req("a")); err != nil || calls.Load() != 3 {
		t.Errorf("429 retry: %v after %d calls", err, calls.Load())
	}

	// circuit breaker: 3 requests exhausting retries → fail fast
	status.Store(529)
	j := client(srv.URL)
	for i := range 3 {
		if _, err := j.Validate(context.Background(), req("a")); err == nil {
			t.Fatalf("529 #%d should fail", i)
		}
	}
	calls.Store(0)
	if _, err := j.Validate(context.Background(), req("a")); !errors.Is(err, ErrUnavailable) || calls.Load() != 0 {
		t.Errorf("breaker: %v, %d calls", err, calls.Load())
	}
}

func TestMissingKey(t *testing.T) {
	j := client("http://unused")
	j.Key = ""
	if _, err := j.Validate(context.Background(), req("a")); !errors.Is(err, ErrAuth) {
		t.Errorf("missing key: %v", err)
	}
}

func TestCredentialsPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	if got, want := CredentialsPath(), filepath.Join(home, ".config", "jevrules", "credentials"); got != want {
		t.Errorf("default: %s, want %s", got, want)
	}
	t.Setenv("XDG_CONFIG_HOME", "relative/dir") // ignored, per the XDG spec
	if got, want := CredentialsPath(), filepath.Join(home, ".config", "jevrules", "credentials"); got != want {
		t.Errorf("relative XDG_CONFIG_HOME: %s, want %s", got, want)
	}
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	if got, want := CredentialsPath(), filepath.Join(xdg, "jevrules", "credentials"); got != want {
		t.Errorf("XDG_CONFIG_HOME: %s, want %s", got, want)
	}
}

func TestLookupKeyFromFile(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("TYPESAFE_API_KEY", "")
	t.Setenv("JEV_API_KEY", "")
	p := filepath.Join(xdg, "jevrules", "credentials")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	for content, want := range map[string]string{
		"bare-key\n":                       "bare-key",
		"# comment\nJEV_API_KEY=k1\n":      "k1",
		"export TYPESAFE_API_KEY=\"k2\"\n": "k2",
		"OTHER=x\n":                        "",
		"":                                 "",
	} {
		os.WriteFile(p, []byte(content), 0o600)
		if got := LookupKey(); got != want {
			t.Errorf("%q: got %q, want %q", content, got, want)
		}
	}
	t.Setenv("JEV_API_KEY", "from-env") // env wins over the file
	if got := LookupKey(); got != "from-env" {
		t.Errorf("env: got %q", got)
	}

	// an existing file without a key is called out in the error
	t.Setenv("JEV_API_KEY", "")
	os.WriteFile(p, nil, 0o600)
	j := client("http://unused")
	j.Key = LookupKey()
	_, err := j.Validate(context.Background(), req("a"))
	if !errors.Is(err, ErrAuth) || !strings.Contains(err.Error(), "no key found in "+p) {
		t.Errorf("empty file: %v", err)
	}
}
