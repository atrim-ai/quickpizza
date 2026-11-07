package util

import (
	"context"
	"log/slog"
	"math/rand"
	"os"
	"strconv"
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
// evaluated, the environment variable is used.
func faultSetting(envVarName string) (string, bool) {
	if faultFlags != nil {
		value, err := faultFlags.StringValue(context.Background(), envVarName, "", openfeature.EvaluationContext{})
		if err == nil {
			return value, value != ""
		}
	}
	return os.LookupEnv(envVarName)
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
