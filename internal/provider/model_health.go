package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/yetone/magpie/internal/appdir"
	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/filememo"
	"github.com/yetone/magpie/internal/redact"
	"github.com/yetone/magpie/internal/steady"
)

// ModelHealthConfig controls opt-in checks of URL/API-key providers. The
// gateway checks all their text models, including ones hidden by an earlier
// check. Subscription and decision providers keep their own checks.
type ModelHealthConfig struct {
	Enabled          bool `json:"enabled"`
	IntervalMinutes  int  `json:"intervalMinutes"`
	FailureThreshold int  `json:"failureThreshold"`
}

type ModelHealthRecord struct {
	Provider  string    `json:"provider"`
	KeyID     string    `json:"keyId"`
	Model     string    `json:"model"`
	Ready     bool      `json:"ready"`
	Protocol  Protocol  `json:"protocol,omitempty"` // last endpoint that answered successfully
	Failures  int       `json:"failures"`
	CheckedAt time.Time `json:"checkedAt"`
	Result    Result    `json:"result"`
}

// ModelHealthState is separate from providers.json: checks never erase the
// user's picks or keys. Records are addressed by a digest of the destination,
// key and model settings, so changing them requires a fresh successful check.
type ModelHealthState struct {
	ModelHealthConfig
	LastScan time.Time                    `json:"lastScan,omitempty"`
	Records  map[string]ModelHealthRecord `json:"records,omitempty"`
}

func ModelHealthPath() string { return filepath.Join(appdir.Config(), "model-health.json") }

// LoadModelHealth returns a copy, or the read error. A corrupt file is never
// treated as disabled or overwritten by a scan.
func LoadModelHealth() (ModelHealthState, error) {
	s, err := modelHealthSnapshot()
	s.Records = cloneHealthRecords(s.Records)
	return s, err
}

func cloneHealthRecords(in map[string]ModelHealthRecord) map[string]ModelHealthRecord {
	out := make(map[string]ModelHealthRecord, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func modelHealthSnapshot() (ModelHealthState, error) {
	s, err := filememo.Read("model-health", ModelHealthPath(), parseModelHealth)
	if errors.Is(err, os.ErrNotExist) {
		return ModelHealthState{ModelHealthConfig: ModelHealthConfig{IntervalMinutes: 30, FailureThreshold: 2}}, nil
	}
	return s, err
}

func readModelHealth() (ModelHealthState, error) {
	s := ModelHealthState{ModelHealthConfig: ModelHealthConfig{IntervalMinutes: 30, FailureThreshold: 2}}
	b, err := steady.ReadFile(ModelHealthPath())
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	return parseModelHealth(b)
}

func parseModelHealth(b []byte) (ModelHealthState, error) {
	s := ModelHealthState{ModelHealthConfig: ModelHealthConfig{IntervalMinutes: 30, FailureThreshold: 2}}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("read %s: %w", ModelHealthPath(), err)
	}
	if err := healthConfigOK(s.ModelHealthConfig); err != nil {
		return s, err
	}
	return s, nil
}

func healthConfigOK(c ModelHealthConfig) error {
	if c.IntervalMinutes < 1 || c.IntervalMinutes > 1440 {
		return errors.New("model health interval must be 1–1440 minutes")
	}
	if c.FailureThreshold < 1 || c.FailureThreshold > 10 {
		return errors.New("model health failure threshold must be 1–10")
	}
	return nil
}

func writeModelHealth(s ModelHealthState) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := writePrivate(ModelHealthPath(), append(b, '\n')); err != nil {
		return err
	}
	catalog.Touched()
	return nil
}

func ConfigureModelHealth(c ModelHealthConfig) error {
	if err := healthConfigOK(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	unlock, err := lockModelHealth(ctx, ".lock", true)
	if err != nil {
		return err
	}
	defer unlock()
	s, err := readModelHealth()
	if err != nil {
		return err
	}
	s.ModelHealthConfig = c
	return writeModelHealth(s)
}

func healthChecks(p Provider, model string) bool {
	return p.Account == nil && !p.DecideOnly() && !p.DecidesModel(model) &&
		(p.Chat != "" || p.Responses != "" || p.Anthropic != "") && !p.drawsOnImages(model)
}

func healthRecordID(p Provider, model string) string {
	// Hash credentials and headers, never put them in the health file.
	b, _ := json.Marshal([]any{p.ID, p.Key, p.KeyProtocol, p.Chat, p.Responses, p.Anthropic,
		p.Headers, p.Proxy, p.Preset, UpstreamName(p, model), p.APIs(model), model})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// ModelHealthAllows is for one selected key, including a key absent from a
// vendor's advertised list. Unknown and quarantined text models are never
// tried while checks are enabled, even as the gateway's last candidate.
func ModelHealthAllows(p Provider, model string) bool {
	if !healthChecks(p, model) {
		return true
	}
	s, err := modelHealthSnapshot()
	return healthAllowsIn(s, err, p, model)
}

func healthAllowsIn(s ModelHealthState, err error, p Provider, model string) bool {
	if err != nil {
		return false
	}
	return !s.Enabled || s.Records[healthRecordID(p, model)].Ready
}

// ModelHealthProtocol confines a verified model to the endpoint that
// actually answered. An untested sibling endpoint may still be broken.
func ModelHealthProtocol(p Provider, model string) Protocol {
	if !healthChecks(p, model) {
		return ""
	}
	s, err := modelHealthSnapshot()
	if err != nil || !s.Enabled {
		return ""
	}
	return s.Records[healthRecordID(p, model)].Protocol
}

func healthExposed(p Provider, ms []catalog.Model) []catalog.Model {
	s, err := modelHealthSnapshot()
	if err == nil && !s.Enabled {
		return ms
	}
	return slices.DeleteFunc(slices.Clone(ms), func(m catalog.Model) bool {
		if !healthChecks(p, m.ID) {
			return false
		}
		keys := p.KeysOn()
		if len(keys) == 0 {
			return !healthAllowsIn(s, err, p, m.ID)
		}
		for _, k := range keys {
			q := p.WithKey(k)
			if len(q.Speaks()) > 0 && p.AccountServes(KeyID(k.Key), m.ID) && healthAllowsIn(s, err, q, m.ID) {
				return false
			}
		}
		return true
	})
}

var ErrModelHealthScanning = errors.New("another magpie is checking models; try again after it finishes")

// lockModelHealth serializes both state writes and whole scans across the
// app and CLI. The OS releases locks on process exit; lock files stay put.
func lockModelHealth(ctx context.Context, suffix string, wait bool) (func(), error) {
	path := ModelHealthPath() + suffix
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		ok, err := tryModelHealthLock(f)
		if err != nil {
			f.Close()
			return nil, err
		}
		if ok {
			return func() { unlockModelHealthFile(f); f.Close() }, nil
		}
		if !wait {
			f.Close()
			return nil, ErrModelHealthScanning
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// ScanModelHealth refreshes advertised lists without deleting picks, and
// checks every text model and enabled key with a global concurrency of four.
// Cancellation never counts as a failed model check.
func ScanModelHealth(ctx context.Context) (ModelHealthState, error) {
	unlock, err := lockModelHealth(ctx, ".scan.lock", false)
	if err != nil {
		return ModelHealthState{}, err
	}
	defer unlock()
	s, err := LoadModelHealth()
	if err != nil {
		return s, err
	}
	if !s.Enabled {
		return s, errors.New("model health checks are off; run magpie provider health on")
	}
	if err := FileError(); err != nil {
		return s, err
	}
	type job struct {
		p     Provider
		model string
	}
	var jobs []job
	for _, p := range All() {
		if !p.On() || p.Account != nil || p.DecideOnly() || p.ModelTest() != "" {
			continue
		}
		fetchCtx, cancel := context.WithTimeout(p.Via(ctx), testWait)
		_, _ = p.fetch(fetchCtx) // keep old list and hand-written picks if listing fails
		cancel()
		if ctx.Err() != nil {
			return s, ctx.Err()
		}
		// Listing may repair an OpenAI base URL by adding /v1. Use the
		// saved destination for both the probe and its credential digest.
		if fresh, err := Find(p.ID); err == nil {
			p = *fresh
		} else {
			continue
		}
		if !p.On() || p.Account != nil || p.DecideOnly() {
			continue
		}
		ms := slices.Clone(p.Available())
		for _, id := range p.Models {
			if !slices.ContainsFunc(ms, func(m catalog.Model) bool { return m.ID == id }) {
				ms = append(ms, catalog.Model{ID: id})
			}
		}
		keys := p.KeysOn()
		if len(keys) == 0 {
			keys = []KeyAccount{{}}
		}
		for _, k := range keys {
			q := p.WithKey(k)
			q.Keys = nil // test this key, never silently choose another
			for _, m := range ms {
				if healthChecks(q, m.ID) && p.AccountServes(KeyID(k.Key), m.ID) {
					jobs = append(jobs, job{q, m.ID})
				}
			}
		}
	}
	var wg sync.WaitGroup
	queue := make(chan job)
	var mu sync.Mutex
	var scanErr error
	for range 4 {
		wg.Go(func() {
			for j := range queue {
				if ctx.Err() != nil {
					continue
				}
				r := j.p.healthTest(ctx, j.model)
				if ctx.Err() != nil {
					continue
				}
				if err := saveHealthResult(ctx, j.p, j.model, r); err != nil {
					mu.Lock()
					scanErr = errors.Join(scanErr, err)
					mu.Unlock()
				}
			}
		})
	}
	for _, j := range jobs {
		select {
		case queue <- j:
		case <-ctx.Done():
		}
	}
	close(queue)
	wg.Wait()
	if ctx.Err() != nil {
		return s, ctx.Err()
	}
	if scanErr != nil {
		return s, scanErr
	}
	unlockWrite, err := lockModelHealth(ctx, ".lock", true)
	if err != nil {
		return s, err
	}
	defer unlockWrite()
	s, err = readModelHealth()
	if err != nil {
		return s, err
	}
	s.LastScan = time.Now()
	err = writeModelHealth(s)
	return s, err
}

func (p Provider) healthTest(ctx context.Context, model string) Result {
	protos := p.Speaks()
	if apis := p.APIs(model); len(apis) > 0 {
		protos = slices.DeleteFunc(slices.Clone(protos), func(pr Protocol) bool { return !slices.Contains(apis, pr) })
	}
	if isClaude(model) && slices.Contains(protos, Anthropic) {
		protos = append([]Protocol{Anthropic}, slices.DeleteFunc(slices.Clone(protos), func(pr Protocol) bool { return pr == Anthropic })...)
	}
	r := Result{Model: model, Error: "no endpoint serves this model"}
	// One working endpoint is enough: the gateway translates onto the
	// recorded protocol. Bound all endpoint attempts to one test timeout.
	ctx, cancel := context.WithTimeout(p.Via(ctx), testWait)
	defer cancel()
	for _, pr := range protos {
		if pr != Chat && pr != Responses && pr != Anthropic {
			continue
		}
		u, body := tiny(p, pr, UpstreamName(p, model))
		r = probeReply(ctx, p, pr, u, p.Prepare([]byte(body)), model, testWait, true)
		if r.OK || ctx.Err() != nil {
			break
		}
	}
	if p.Key != "" {
		r.Error = strings.ReplaceAll(r.Error, p.Key, "[redacted]")
	}
	for name, value := range p.Headers {
		if value != "" && redact.ScrubHeader(name, value) == redact.Scrubbed {
			r.Error = strings.ReplaceAll(r.Error, value, "[redacted]")
			if _, credential, ok := strings.Cut(value, " "); ok && credential != "" {
				r.Error = strings.ReplaceAll(r.Error, credential, "[redacted]")
			}
		}
	}
	r.Error = redact.Scrub(r.Error)
	if len(r.Error) > 1024 {
		r.Error = r.Error[:1024]
	}
	return r
}

func saveHealthResult(ctx context.Context, p Provider, model string, result Result) error {
	unlock, err := lockModelHealth(ctx, ".lock", true)
	if err != nil {
		return err
	}
	defer unlock()
	s, err := readModelHealth()
	if err != nil {
		return err
	}
	if !s.Enabled {
		return nil
	} // disabling during a scan takes effect immediately
	if s.Records == nil {
		s.Records = map[string]ModelHealthRecord{}
	}
	id := healthRecordID(p, model)
	r := s.Records[id]
	r.Provider, r.KeyID, r.Model, r.CheckedAt, r.Result = p.ID, KeyID(p.Key), model, time.Now(), result
	if result.OK {
		r.Ready, r.Failures = true, 0
		r.Protocol = result.Protocol
	} else {
		r.Failures++
		if r.Failures >= s.FailureThreshold {
			r.Ready = false
		}
	}
	s.Records[id] = r
	return writeModelHealth(s)
}

// KeepModelsHealthy wakes every minute and runs overdue scans. Newly added
// keys and models wait for the next scan, or an explicit CLI scan.
func KeepModelsHealthy(ctx context.Context) {
	for {
		s, err := LoadModelHealth()
		if err == nil && s.Enabled && time.Since(s.LastScan) >= time.Duration(s.IntervalMinutes)*time.Minute {
			if _, err := ScanModelHealth(ctx); err != nil && ctx.Err() == nil && !errors.Is(err, ErrModelHealthScanning) {
				log.Printf("model health: %v", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Minute):
		}
	}
}
