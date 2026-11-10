package util

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/open-feature/go-sdk/openfeature"
)

// failingProvider fails every evaluation with the given resolution error, standing in for an unreachable flagd.
type failingProvider struct {
	err openfeature.ResolutionError
}

func (p failingProvider) Metadata() openfeature.Metadata {
	return openfeature.Metadata{Name: "failing"}
}
func (p failingProvider) Hooks() []openfeature.Hook { return nil }
func (p failingProvider) BooleanEvaluation(_ context.Context, _ string, d bool, _ openfeature.FlattenedContext) openfeature.BoolResolutionDetail {
	return openfeature.BoolResolutionDetail{Value: d, ProviderResolutionDetail: openfeature.ProviderResolutionDetail{ResolutionError: p.err}}
}
func (p failingProvider) StringEvaluation(_ context.Context, _ string, d string, _ openfeature.FlattenedContext) openfeature.StringResolutionDetail {
	return openfeature.StringResolutionDetail{Value: d, ProviderResolutionDetail: openfeature.ProviderResolutionDetail{ResolutionError: p.err}}
}
func (p failingProvider) FloatEvaluation(_ context.Context, _ string, d float64, _ openfeature.FlattenedContext) openfeature.FloatResolutionDetail {
	return openfeature.FloatResolutionDetail{Value: d, ProviderResolutionDetail: openfeature.ProviderResolutionDetail{ResolutionError: p.err}}
}
func (p failingProvider) IntEvaluation(_ context.Context, _ string, d int64, _ openfeature.FlattenedContext) openfeature.IntResolutionDetail {
	return openfeature.IntResolutionDetail{Value: d, ProviderResolutionDetail: openfeature.ProviderResolutionDetail{ResolutionError: p.err}}
}
func (p failingProvider) ObjectEvaluation(_ context.Context, _ string, d any, _ openfeature.FlattenedContext) openfeature.InterfaceResolutionDetail {
	return openfeature.InterfaceResolutionDetail{Value: d, ProviderResolutionDetail: openfeature.ProviderResolutionDetail{ResolutionError: p.err}}
}

// fakeClock is a settable clock for the limiter.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

// withFailingFlags points faultSetting at a provider that fails with err, captures the default logger at debug,
// and installs a limiter on a fake clock. It returns the log buffer and the clock.
func withFailingFlags(t *testing.T, err openfeature.ResolutionError) (*bytes.Buffer, *fakeClock) {
	t.Helper()
	domain := "test-" + t.Name()
	if e := openfeature.SetNamedProviderAndWait(domain, failingProvider{err: err}); e != nil {
		t.Fatalf("setting provider: %v", e)
	}
	prevFlags, prevLog, prevLogger := faultFlags, faultReadLog, slog.Default()
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	faultFlags = openfeature.NewClient(domain)
	faultReadLog = newFaultReadLimiter(time.Minute, clock.now)
	t.Cleanup(func() {
		faultFlags, faultReadLog = prevFlags, prevLog
		slog.SetDefault(prevLogger)
	})
	return &buf, clock
}

func logLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("log line is not JSON: %q", l)
		}
		lines = append(lines, m)
	}
	return lines
}

func TestFaultSettingLogsFailedReadAndFallsBackToEnv(t *testing.T) {
	const key = "QUICKPIZZA_FAIL_RATE_TEST"
	t.Setenv(key, "25")
	buf, _ := withFailingFlags(t, openfeature.NewProviderNotReadyResolutionError("connection refused"))

	value, ok := faultSetting(key)
	if !ok || value != "25" {
		t.Fatalf("faultSetting = (%q, %v), want the env var (\"25\", true)", value, ok)
	}
	lines := logLines(t, buf)
	if len(lines) != 1 {
		t.Fatalf("got %d log lines, want 1: %s", len(lines), buf)
	}
	l := lines[0]
	if l["level"] != "WARN" || l["flag"] != key || l["error_code"] != "PROVIDER_NOT_READY" || l["env_set"] != true {
		t.Fatalf("unexpected log line: %v", l)
	}
	if !strings.Contains(l["err"].(string), "connection refused") {
		t.Fatalf("log line does not carry the error: %v", l)
	}
}

func TestFaultSettingFailedReadIsRateLimitedPerFlag(t *testing.T) {
	const a, b = "QUICKPIZZA_DELAY_A", "QUICKPIZZA_DELAY_B"
	buf, clock := withFailingFlags(t, openfeature.NewGeneralResolutionError("unimplemented"))

	for i := 0; i < 100; i++ {
		if _, ok := faultSetting(a); ok {
			t.Fatalf("unset env var read as set")
		}
	}
	faultSetting(b) // a different flag is not suppressed by a's window
	clock.t = clock.t.Add(59 * time.Second)
	faultSetting(a) // still inside a's window
	clock.t = clock.t.Add(time.Second)
	faultSetting(a) // window elapsed: logs again, with the count it suppressed

	lines := logLines(t, buf)
	if len(lines) != 3 {
		t.Fatalf("got %d log lines, want 3: %s", len(lines), buf)
	}
	if lines[0]["flag"] != a || lines[0]["suppressed"] != float64(0) {
		t.Fatalf("first line: %v", lines[0])
	}
	if lines[1]["flag"] != b {
		t.Fatalf("second line: %v", lines[1])
	}
	if lines[2]["flag"] != a || lines[2]["suppressed"] != float64(100) {
		t.Fatalf("third line should report 100 suppressed reads of %s: %v", a, lines[2])
	}
}

func TestFaultSettingUndefinedFlagLogsAtDebug(t *testing.T) {
	const key = "QUICKPIZZA_DELAY_UNDEFINED"
	buf, _ := withFailingFlags(t, openfeature.NewFlagNotFoundResolutionError("flag not found"))

	if _, ok := faultSetting(key); ok {
		t.Fatalf("unset env var read as set")
	}
	lines := logLines(t, buf)
	if len(lines) != 1 || lines[0]["level"] != "DEBUG" || lines[0]["error_code"] != "FLAG_NOT_FOUND" {
		t.Fatalf("want one DEBUG FLAG_NOT_FOUND line, got: %s", buf)
	}
}

func TestFaultSettingWithoutFlagdReadsEnvSilently(t *testing.T) {
	const key = "QUICKPIZZA_DELAY_ENV_ONLY"
	t.Setenv(key, "1s")
	prevFlags, prevLogger := faultFlags, slog.Default()
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	faultFlags = nil
	t.Cleanup(func() { faultFlags = prevFlags; slog.SetDefault(prevLogger) })

	if v, ok := faultSetting(key); !ok || v != "1s" {
		t.Fatalf("faultSetting = (%q, %v)", v, ok)
	}
	if buf.Len() != 0 {
		t.Fatalf("no flagd configured should not log: %s", buf.String())
	}
}
