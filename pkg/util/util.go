package util

import (
	"context"
	"log/slog"
	"math/rand"
	"os"
	"strconv"
	"sync"
	"time"

	flagd "github.com/open-feature/go-sdk-contrib/providers/flagd/pkg"
	"github.com/open-feature/go-sdk/openfeature"
)

const (
	MaxUserNameLength = 32
)

var characters = []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789")

func GenerateAlphaNumToken(length int) string {
	data := make([]rune, length)
	for i := range data {
		// NOTE: This should use a cryptographically-safe random
		// number generator instead.
		data[i] = characters[rand.Intn(len(characters))]
	}
	return string(data)
}

// faultFlags evaluates fault settings from flagd. It is nil unless InitFaultFlags found QUICKPIZZA_FLAGD_HOST.
var faultFlags *openfeature.Client

// InitFaultFlags connects the fault helpers below to flagd when QUICKPIZZA_FLAGD_HOST is set, so faults can be
// switched at runtime without restarting the pod. The port comes from FLAGD_PORT (default 8013).
// The provider cache is disabled: a cached answer can outlive a flag change when the provider starts before
// flagd is reachable.
func InitFaultFlags() {
	host, ok := os.LookupEnv("QUICKPIZZA_FLAGD_HOST")
	if !ok || host == "" {
		return
	}
	provider, err := flagd.NewProvider(flagd.WithHost(host), flagd.WithoutCache())
	if err != nil {
		slog.Error("creating flagd provider, fault flags disabled", "err", err)
		return
	}
	// SetProvider does not block; until the provider is ready, evaluations fail and fall back to the environment.
	if err := openfeature.SetProvider(provider); err != nil {
		slog.Error("setting flagd provider, fault flags disabled", "err", err)
		return
	}
	faultFlags = openfeature.NewClient("quickpizza")
	slog.Info("fault flags enabled", "flagd_host", host)
}

// faultSetting returns the value of the fault named by envVarName and whether it is set. A flagd string flag of
// the same name takes precedence, with "" meaning off; if flagd is not configured or the flag cannot be
// evaluated, the environment variable is used. A failed evaluation is logged (see faultReadLog) before the
// fallback, so a flagd that is down, out of memory or missing the evaluation RPC does not silently read as the
// env var (usually: off) while flagd itself shows the fault on.
func faultSetting(envVarName string) (string, bool) {
	if faultFlags != nil {
		details, err := faultFlags.StringValueDetails(context.Background(), envVarName, "", openfeature.EvaluationContext{})
		if err == nil {
			return details.Value, details.Value != ""
		}
		value, ok := os.LookupEnv(envVarName)
		faultReadLog.failed(envVarName, details.ErrorCode, err, ok)
		return value, ok
	}
	return os.LookupEnv(envVarName)
}

// faultReadLogInterval is the minimum time between two log lines for failed reads of the same flag.
const faultReadLogInterval = time.Minute

// faultReadLog logs failed fault reads, at most once per flag per faultReadLogInterval.
var faultReadLog = newFaultReadLimiter(faultReadLogInterval, time.Now)

// faultReadLimiter rate-limits the failed-read log per flag key. Faults are read on every request, so an unlimited
// line would flood the log for as long as flagd is unreachable. Each line carries the number of failures
// suppressed since the previous line for that flag.
type faultReadLimiter struct {
	interval time.Duration
	now      func() time.Time

	mu         sync.Mutex
	last       map[string]time.Time
	suppressed map[string]int
}

func newFaultReadLimiter(interval time.Duration, now func() time.Time) *faultReadLimiter {
	return &faultReadLimiter{interval: interval, now: now, last: map[string]time.Time{}, suppressed: map[string]int{}}
}

// allow reports whether a failed read of key may be logged now and, if so, how many were suppressed before it.
func (l *faultReadLimiter) allow(key string) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if last, seen := l.last[key]; seen && now.Sub(last) < l.interval {
		l.suppressed[key]++
		return false, 0
	}
	n := l.suppressed[key]
	l.last[key] = now
	l.suppressed[key] = 0
	return true, n
}

// failed logs a failed evaluation of the fault flag key. FLAG_NOT_FOUND is the documented "flagd does not define
// this fault, use the env var" path and logs at debug; every other code (flagd unreachable, provider not ready,
// a missing RPC) logs a warning.
func (l *faultReadLimiter) failed(key string, code openfeature.ErrorCode, err error, envSet bool) {
	ok, suppressed := l.allow(key)
	if !ok {
		return
	}
	level := slog.LevelWarn
	if code == openfeature.FlagNotFoundCode {
		level = slog.LevelDebug
	}
	slog.Log(context.Background(), level, "fault flag read failed, falling back to the env var",
		"flag", key, "error_code", string(code), "err", err, "env_set", envSet,
		"suppressed", suppressed, "log_interval", l.interval.String())
}

// DelayIfEnvSet sleeps for the duration specified by the environment variable (or the flagd flag of the same name).
// The value must be a Go duration string (e.g. "500ms", "2s"). Invalid values are ignored.
func DelayIfEnvSet(envVarName string) {
	delayStr, ok := faultSetting(envVarName)
	if !ok {
		return
	}
	d, err := time.ParseDuration(delayStr)
	if err != nil {
		slog.Warn("invalid duration for env var, ignoring", "env", envVarName, "value", delayStr)
		return
	}
	time.Sleep(d)
}

// BurnCPUIfEnvSet keeps the calling goroutine busy on the CPU for the duration specified by the environment
// variable (or the flagd flag of the same name). The value must be a Go duration string. Invalid values are ignored.
func BurnCPUIfEnvSet(envVarName string) {
	burnStr, ok := faultSetting(envVarName)
	if !ok {
		return
	}
	d, err := time.ParseDuration(burnStr)
	if err != nil {
		slog.Warn("invalid duration for env var, ignoring", "env", envVarName, "value", burnStr)
		return
	}
	x := uint64(1)
	for deadline := time.Now().Add(d); time.Now().Before(deadline); {
		for i := 0; i < 1000; i++ {
			x = x*6364136223846793005 + 1442695040888963407
		}
	}
	_ = x
}

// FailRandomlyIfEnvSet checks if this request should fail based on a random percentage.
// The environment variable (or the flagd flag of the same name) should contain an integer value between 0 and 100
// representing the failure rate percentage. Invalid values are treated as 0 (no failures).
func FailRandomlyIfEnvSet(envVarName string) bool {
	rateStr, ok := faultSetting(envVarName)
	if !ok {
		return false
	}

	rate, err := strconv.Atoi(rateStr)
	if err != nil || rate < 0 || rate > 100 {
		return false
	}

	return rand.Intn(100) < rate
}
