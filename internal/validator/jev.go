package validator

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/zstd"
)

const Endpoint = "https://api.typesafe.ai/v1/systemone"

type JEV struct {
	Endpoint string
	Key      string
	model    string
	HTTP     *http.Client
	MaxTries int
	// Sleep is replaceable in tests.
	Sleep func(time.Duration)
	// failures counts consecutive requests that failed after all retries;
	// at breakerLimit every further call fails fast.
	failures atomic.Int32
}

const breakerLimit = 3

// NewJEV builds a client. The key may be empty: the error surfaces on the
// first real call, so fully cached runs work without one.
func NewJEV(model string) *JEV {
	ep := Endpoint
	if v := os.Getenv("JEVRULES_JEV_ENDPOINT"); v != "" { // tests, proxies
		ep = v
	}
	return &JEV{
		Endpoint: ep,
		Key:      LookupKey(),
		model:    model,
		HTTP:     &http.Client{Timeout: 120 * time.Second},
		MaxTries: 5,
		Sleep:    time.Sleep,
	}
}

func (j *JEV) Name() string  { return "jev" }
func (j *JEV) Model() string { return j.model }

// CredentialsPath is $XDG_CONFIG_HOME/jevrules/credentials, falling back to
// ~/.config/jevrules/credentials (on macOS too: CLI tools use ~/.config, not
// ~/Library). It returns "" when there is no home directory.
func CredentialsPath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if !filepath.IsAbs(dir) { // unset or relative: ignored, per the XDG spec
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "jevrules", "credentials")
}

// missingKey says where to put the key, and whether the credentials file
// exists but holds no key.
func missingKey() string {
	p := CredentialsPath()
	if p == "" {
		return "set TYPESAFE_API_KEY"
	}
	if _, err := os.Stat(p); err == nil {
		return fmt.Sprintf("no key found in %s (expected the bare key or TYPESAFE_API_KEY=…), and TYPESAFE_API_KEY is not set", p)
	}
	return fmt.Sprintf("put it in %s or set TYPESAFE_API_KEY", p)
}

// LookupKey checks TYPESAFE_API_KEY, JEV_API_KEY, then the CredentialsPath
// file (either the bare key or KEY=value lines).
// The key is never logged, cached or reported.
func LookupKey() string {
	for _, v := range []string{"TYPESAFE_API_KEY", "JEV_API_KEY"} {
		if k := strings.TrimSpace(os.Getenv(v)); k != "" {
			return k
		}
	}
	p := CredentialsPath()
	if p == "" {
		return ""
	}
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if k, v, ok := strings.Cut(l, "="); ok {
			k = strings.TrimSpace(strings.TrimPrefix(k, "export "))
			if k == "TYPESAFE_API_KEY" || k == "JEV_API_KEY" {
				return strings.Trim(strings.TrimSpace(v), `"'`)
			}
			continue
		}
		return l
	}
	return ""
}

type apiRequest struct {
	Model     string              `json:"model"`
	State     State               `json:"state"`
	Questions map[string]Question `json:"questions"`
}

type apiResponse struct {
	Model   string                     `json:"model"`
	Answers map[string]json.RawMessage `json:"answers"`
	Usage   struct {
		InputTokens int `json:"input_tokens"`
	} `json:"usage"`
}

var zenc, _ = zstd.NewWriter(nil, zstd.WithWindowSize(8<<20), zstd.WithEncoderConcurrency(1))

func (j *JEV) Validate(ctx context.Context, req Request) (Result, error) {
	if j.Key == "" {
		return Result{}, fmt.Errorf("%w: %s", ErrAuth, missingKey())
	}
	body, err := json.Marshal(apiRequest{Model: j.model, State: req.State, Questions: req.Questions})
	if err != nil {
		return Result{}, err
	}
	if j.failures.Load() >= breakerLimit {
		return Result{}, fmt.Errorf("%w: %d requests in a row failed", ErrUnavailable, breakerLimit)
	}
	encoding := ""
	if len(body) >= 1024 {
		body = zenc.EncodeAll(body, nil)
		encoding = "zstd"
	}

	var lastErr error
	for try := 0; try < j.MaxTries; try++ {
		if try > 0 {
			if err := ctx.Err(); err != nil {
				return Result{}, err
			}
		}
		res, retryAfter, err := j.do(ctx, body, encoding, req.Questions)
		if err == nil {
			j.failures.Store(0)
			return res, nil
		}
		lastErr = err
		if retryAfter < 0 { // not retryable
			return Result{}, err
		}
		backoff := time.Duration(500*(1<<try)) * time.Millisecond
		backoff += time.Duration(rand.Int64N(int64(backoff/2) + 1))
		j.Sleep(max(backoff, retryAfter))
	}
	if j.failures.Add(1) >= breakerLimit {
		return Result{}, fmt.Errorf("%w: giving up after %d tries: %w", ErrUnavailable, j.MaxTries, lastErr)
	}
	return Result{}, fmt.Errorf("giving up after %d tries: %w", j.MaxTries, lastErr)
}

// do performs one attempt. retryAfter < 0 means the error is final.
func (j *JEV) do(ctx context.Context, body []byte, encoding string, qs map[string]Question) (Result, time.Duration, error) {
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, j.Endpoint, bytes.NewReader(body))
	if err != nil {
		return Result{}, -1, err
	}
	hreq.Header.Set("Authorization", "Bearer "+j.Key)
	hreq.Header.Set("Content-Type", "application/json")
	if encoding != "" {
		hreq.Header.Set("Content-Encoding", encoding)
	}
	resp, err := j.HTTP.Do(hreq)
	if err != nil {
		if ctx.Err() != nil {
			return Result{}, -1, ctx.Err()
		}
		return Result{}, 0, err // network error: retry
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))

	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return Result{}, -1, fmt.Errorf("%w (HTTP %d)", ErrAuth, resp.StatusCode)
	case resp.StatusCode == http.StatusRequestEntityTooLarge,
		resp.StatusCode == http.StatusBadRequest && bytes.Contains(data, []byte("max_tokens_exceeded")):
		return Result{}, -1, fmt.Errorf("%w: %s", ErrTooLarge, snippet(data))
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == 529 || resp.StatusCode >= 500:
		ra := time.Duration(0)
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
			ra = time.Duration(s) * time.Second
		}
		return Result{}, ra, fmt.Errorf("JEV HTTP %d: %s", resp.StatusCode, snippet(data))
	default:
		return Result{}, -1, fmt.Errorf("JEV HTTP %d: %s", resp.StatusCode, snippet(data))
	}

	var ar apiResponse
	if err := json.Unmarshal(data, &ar); err != nil {
		return Result{}, -1, fmt.Errorf("JEV response: %w", err)
	}
	res := Result{Probabilities: map[string]float64{}, Choices: map[string]map[string]float64{}, InputTokens: ar.Usage.InputTokens, Model: ar.Model}
	for _, id := range sortedKeys(qs) {
		raw, ok := ar.Answers[id]
		if !ok {
			return Result{}, -1, fmt.Errorf("JEV response has no answer for %q", id)
		}
		if qs[id].Raw {
			var a struct {
				Probabilities map[string]float64 `json:"probabilities"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return Result{}, -1, err
			}
			res.Choices[id] = a.Probabilities
			continue
		}
		p, err := violation(qs[id], raw)
		if err != nil {
			return Result{}, -1, err
		}
		res.Probabilities[id] = p
	}
	return res, 0, nil
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
