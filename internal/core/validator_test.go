package core

import (
	"context"
	"errors"
	"io"
	"log"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testValidatorConfig() ValidatorConfig {
	return ValidatorConfig{
		ProbeTimeout:         time.Second,
		Concurrency:          4,
		CheckInterval:        10 * time.Millisecond,
		StaleAfter:           20 * time.Millisecond,
		MaxFails:             3,
		BackoffBase:          time.Millisecond,
		BackoffMax:           time.Millisecond,
		TrafficFailThreshold: 3,
	}
}

func newTestValidator(t *testing.T) (*ProxyValidator, *graduateRecorder) {
	t.Helper()
	rec := &graduateRecorder{}
	v := NewProxyValidator(log.New(io.Discard, "", 0), testValidatorConfig())
	v.SetGraduate(rec.record)
	return v, rec
}

type graduateRecorder struct {
	mu        sync.Mutex
	graduates [][]string
}

func (r *graduateRecorder) record(ready []*ProxyState) {
	keys := make([]string, 0, len(ready))
	for _, ps := range ready {
		keys = append(keys, ps.Key)
	}
	r.mu.Lock()
	r.graduates = append(r.graduates, keys)
	r.mu.Unlock()
}

func (r *graduateRecorder) last() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.graduates) == 0 {
		return nil
	}
	return r.graduates[len(r.graduates)-1]
}

func (r *graduateRecorder) contains(key string) bool {
	for _, k := range r.last() {
		if k == key {
			return true
		}
	}
	return false
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func validatorReady(v *ProxyValidator, key string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	_, ok := v.ready[key]
	return ok
}

func validatorQuarantined(v *ProxyValidator, key string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	_, ok := v.quarantine[key]
	return ok
}

func validatorHas(v *ProxyValidator, key string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	_, inReady := v.ready[key]
	_, inPending := v.pending[key]
	_, inQuarantine := v.quarantine[key]
	return inReady || inPending || inQuarantine
}

func TestValidatorGraduatesOnlyHealthy(t *testing.T) {
	v, rec := newTestValidator(t)
	v.Start()

	good := &ProxyState{Key: "good", ProbeFunc: okProbe}
	bad := &ProxyState{Key: "bad", ProbeFunc: failProbe}
	v.Submit([]*ProxyState{good, bad})

	waitFor(t, "good to graduate", func() bool { return validatorReady(v, "good") })
	if !rec.contains("good") {
		t.Fatalf("good proxy never graduated, last ready set %v", rec.last())
	}
	if validatorReady(v, "bad") || rec.contains("bad") {
		t.Fatal("failing proxy must not graduate")
	}
}

func TestValidatorReProbeDemotes(t *testing.T) {
	v, rec := newTestValidator(t)
	v.Start()

	var healthy atomic.Bool
	healthy.Store(true)
	ps := &ProxyState{Key: "flaky", ProbeFunc: func(ctx context.Context) (time.Duration, error) {
		if healthy.Load() {
			return time.Millisecond, nil
		}
		return 0, errors.New("dead")
	}}
	v.Submit([]*ProxyState{ps})

	waitFor(t, "proxy to graduate", func() bool { return validatorReady(v, "flaky") })

	healthy.Store(false)
	waitFor(t, "proxy to be demoted", func() bool { return validatorQuarantined(v, "flaky") })
	if validatorReady(v, "flaky") {
		t.Fatal("proxy still ready after probe failure")
	}
	if rec.contains("flaky") {
		t.Fatalf("demoted proxy still in last graduate set %v", rec.last())
	}
}

func TestValidatorQuarantineRecovers(t *testing.T) {
	v, _ := newTestValidator(t)
	v.Start()

	var healthy atomic.Bool
	ps := &ProxyState{Key: "recover", ProbeFunc: func(ctx context.Context) (time.Duration, error) {
		if healthy.Load() {
			return time.Millisecond, nil
		}
		return 0, errors.New("dead")
	}}
	v.Submit([]*ProxyState{ps})

	waitFor(t, "proxy to be quarantined", func() bool { return validatorQuarantined(v, "recover") })

	healthy.Store(true)
	waitFor(t, "proxy to recover to ready", func() bool { return validatorReady(v, "recover") })
}

func TestValidatorEvictsAfterMaxFails(t *testing.T) {
	v, _ := newTestValidator(t)
	v.Start()

	v.Submit([]*ProxyState{&ProxyState{Key: "dead", ProbeFunc: failProbe}})

	// It starts pending, moves to quarantine on the first probe failure,
	// then is evicted after MaxFails consecutive retry failures.
	waitFor(t, "proxy to be quarantined", func() bool { return validatorQuarantined(v, "dead") })
	waitFor(t, "proxy to be evicted", func() bool { return !validatorHas(v, "dead") })
}

func TestValidatorTrafficFailureDemotes(t *testing.T) {
	v, rec := newTestValidator(t)
	v.Start()

	v.Submit([]*ProxyState{&ProxyState{Key: "busy", ProbeFunc: okProbe}})
	waitFor(t, "proxy to graduate", func() bool { return validatorReady(v, "busy") })

	v.OnTrafficFailure("busy")
	waitFor(t, "proxy to be demoted by traffic", func() bool { return validatorQuarantined(v, "busy") })
	if rec.contains("busy") {
		t.Fatalf("demoted proxy still in last graduate set %v", rec.last())
	}
}

func TestValidatorRefreshKeepsReadyKeys(t *testing.T) {
	v, _ := newTestValidator(t)
	v.Start()

	ps := &ProxyState{Key: "stable", ProbeFunc: okProbe}
	v.Submit([]*ProxyState{ps})
	waitFor(t, "proxy to graduate", func() bool { return validatorReady(v, "stable") })

	// A refresh submitting the same key must not drop it from rotation.
	ps2 := &ProxyState{Key: "stable", ProbeFunc: okProbe}
	v.Submit([]*ProxyState{ps2})
	if !validatorReady(v, "stable") {
		t.Fatal("ready proxy dropped by refresh submit")
	}
}

func okProbe(ctx context.Context) (time.Duration, error) {
	return time.Millisecond, nil
}

func failProbe(ctx context.Context) (time.Duration, error) {
	return 0, errors.New("dead")
}
