// Package config loads risk.toml, trading.toml and strategies.toml, ported
// from agent/config.py.
//
// Every Load* call reads from disk, so editing a file takes effect on the next
// loop iteration (the executor re-reads each pass) — same hot-reload contract
// as Python.
//
// Unlike Python's untyped dicts, decoding is strict: a key the structs below
// do not know is an error. That catches the bug class found on 2026-11-09,
// where adopt_protective_stop_pct / adopt_untracked_drift sat under the
// [per_trade.confidence_scale] header instead of [per_trade]; the reconciler
// read them from per_trade, never found them, and silently used its defaults.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

const (
	RiskFile       = "risk.toml"
	TradingFile    = "trading.toml"
	StrategiesFile = "strategies.toml"
)

// Loader reads the three config files from Dir.
type Loader struct{ Dir string }

// ── risk.toml ────────────────────────────────────────────────────────────

// Risk is risk.toml: the portfolio risk engine's limits.
type Risk struct {
	Portfolio      Portfolio      `toml:"portfolio"`
	PerTrade       PerTrade       `toml:"per_trade"`
	Clusters       Clusters       `toml:"clusters"`
	Direction      Direction      `toml:"direction"`
	Equity         Equity         `toml:"equity"`
	CircuitBreaker CircuitBreaker `toml:"circuit_breaker"`
	Priority       map[string]int `toml:"priority"` // user / event / technical …
}

type Portfolio struct {
	MaxConcurrentPositions int     `toml:"max_concurrent_positions"`
	MaxGrossExposurePct    float64 `toml:"max_gross_exposure_pct"`
	MaxOpenRiskPct         float64 `toml:"max_open_risk_pct"`
	MaxPerSymbolPct        float64 `toml:"max_per_symbol_pct"`
}

type PerTrade struct {
	MaxRiskPct              float64         `toml:"max_risk_pct"`
	VolumeRatioFloor        float64         `toml:"volume_ratio_floor"`
	SessionCloseBlackoutMin float64         `toml:"session_close_blackout_min"`
	RevengeCooldownMin      float64         `toml:"revenge_cooldown_min"`
	MinStopDistancePct      float64         `toml:"min_stop_distance_pct"`
	AdoptProtectiveStopPct  float64         `toml:"adopt_protective_stop_pct"`
	AdoptUntrackedDrift     bool            `toml:"adopt_untracked_drift"`
	ConfidenceScale         ConfidenceScale `toml:"confidence_scale"`
}

// ConfidenceScale shrinks (never grows) the approved quantity by confidence:
// conf >= FullAt gets the full size, <= FloorAt gets FloorScale, linear between.
type ConfidenceScale struct {
	FullAt     float64 `toml:"full_at"`
	FloorAt    float64 `toml:"floor_at"`
	FloorScale float64 `toml:"floor_scale"`
}

type Direction struct {
	AllowShort  bool `toml:"allow_short"`
	AllowMargin bool `toml:"allow_margin"`
}

// Equity: Mode "fixed" trades only the allocated sleeve; anything else syncs
// equity to the broker's net assets (Python: get("mode") == "fixed").
type Equity struct {
	Mode      string  `toml:"mode"`
	SleeveUSD float64 `toml:"sleeve_usd"`
}

// Fixed reports whether equity stays pinned to the sleeve.
func (e Equity) Fixed() bool { return e.Mode == "fixed" }

type CircuitBreaker struct {
	DailyLossLimitPct float64 `toml:"daily_loss_limit_pct"`
	FlattenOnHalt     bool    `toml:"flatten_on_halt"`
}

// Clusters groups correlated symbols that share one notional limit. In TOML
// it is a table mixing symbol arrays with the max_per_cluster_pct number.
type Clusters struct {
	Groups           map[string][]string
	MaxPerClusterPct float64
}

// UnmarshalTOML implements toml.Unmarshaler for the mixed [clusters] table.
func (c *Clusters) UnmarshalTOML(v any) error {
	m, ok := v.(map[string]any)
	if !ok {
		return fmt.Errorf("[clusters] must be a table, got %T", v)
	}
	c.Groups = map[string][]string{}
	for k, raw := range m {
		switch val := raw.(type) {
		case []any:
			syms := make([]string, 0, len(val))
			for _, s := range val {
				str, ok := s.(string)
				if !ok {
					return fmt.Errorf("clusters.%s: symbol %v is not a string", k, s)
				}
				syms = append(syms, str)
			}
			c.Groups[k] = syms
		case int64:
			if k != "max_per_cluster_pct" {
				return fmt.Errorf("clusters.%s: unknown numeric key", k)
			}
			c.MaxPerClusterPct = float64(val)
		case float64:
			if k != "max_per_cluster_pct" {
				return fmt.Errorf("clusters.%s: unknown numeric key", k)
			}
			c.MaxPerClusterPct = val
		default:
			return fmt.Errorf("clusters.%s: unsupported value %T", k, raw)
		}
	}
	return nil
}

// ClusterOf maps symbol → cluster name. Python cached this forever
// (lru_cache), so cluster edits needed a restart; here it follows the file.
func (r *Risk) ClusterOf() map[string]string {
	out := map[string]string{}
	for name, syms := range r.Clusters.Groups {
		for _, s := range syms {
			out[s] = name
		}
	}
	return out
}

func defaultRisk() Risk {
	return Risk{
		PerTrade: PerTrade{
			AdoptProtectiveStopPct: 3, // reconciler.py default
			ConfidenceScale:        ConfidenceScale{FullAt: 75, FloorAt: 50, FloorScale: 0.5},
		},
	}
}

// Validate checks values a typo could turn dangerous.
func (r *Risk) Validate() error {
	var errs []error
	pct := func(name string, v, lo, hi float64) {
		if v < lo || v > hi {
			errs = append(errs, fmt.Errorf("%s = %v, want %v..%v", name, v, lo, hi))
		}
	}
	pct("portfolio.max_gross_exposure_pct", r.Portfolio.MaxGrossExposurePct, 0, 400)
	pct("portfolio.max_open_risk_pct", r.Portfolio.MaxOpenRiskPct, 0, 100)
	pct("portfolio.max_per_symbol_pct", r.Portfolio.MaxPerSymbolPct, 0, 400)
	pct("per_trade.max_risk_pct", r.PerTrade.MaxRiskPct, 0, 100)
	pct("per_trade.min_stop_distance_pct", r.PerTrade.MinStopDistancePct, 0, 100)
	pct("per_trade.adopt_protective_stop_pct", r.PerTrade.AdoptProtectiveStopPct, 0, 100)
	pct("per_trade.confidence_scale.floor_scale", r.PerTrade.ConfidenceScale.FloorScale, 0, 1)
	pct("circuit_breaker.daily_loss_limit_pct", r.CircuitBreaker.DailyLossLimitPct, 0, 100)
	pct("clusters.max_per_cluster_pct", r.Clusters.MaxPerClusterPct, 0, 400)
	if cs := r.PerTrade.ConfidenceScale; cs.FloorAt > cs.FullAt {
		errs = append(errs, fmt.Errorf("confidence_scale.floor_at %v > full_at %v", cs.FloorAt, cs.FullAt))
	}
	if r.Portfolio.MaxConcurrentPositions < 0 {
		errs = append(errs, errors.New("portfolio.max_concurrent_positions < 0"))
	}
	seen := map[string]string{}
	for _, name := range sortedKeys(r.Clusters.Groups) {
		for _, s := range r.Clusters.Groups[name] {
			if prev, dup := seen[s]; dup {
				errs = append(errs, fmt.Errorf("%s is in clusters %s and %s", s, prev, name))
			}
			seen[s] = name
		}
	}
	if r.Equity.Fixed() && r.Equity.SleeveUSD <= 0 {
		errs = append(errs, errors.New("equity.mode=fixed needs sleeve_usd > 0"))
	}
	for _, src := range []string{"user", "event", "technical"} {
		if _, ok := r.Priority[src]; !ok {
			errs = append(errs, fmt.Errorf("priority.%s missing", src))
		}
	}
	return errors.Join(errs...)
}

// ── trading.toml ─────────────────────────────────────────────────────────

// Trading is trading.toml: universe, execution, models and cadence.
type Trading struct {
	Universe  Universe             `toml:"universe"`
	Execution Execution            `toml:"execution"`
	Models    map[string]ModelSpec `toml:"models"` // tier → provider/model
	Cadence   Cadence              `toml:"cadence"`
}

type Universe struct {
	Symbols []string `toml:"symbols"`
	Market  []string `toml:"market"` // regime indicators, never traded
}

type Execution struct {
	CommissionPerShare float64 `toml:"commission_per_share"`
	// EntryOrderTTLMin cancels an unfilled entry limit order after this many
	// minutes. <= 0 disables. Exits never expire.
	EntryOrderTTLMin float64 `toml:"entry_order_ttl_min"`
}

// ModelSpec pins one tier to a provider and model. Model must never be empty:
// an empty model once followed the claude CLI default and burned Fable.
type ModelSpec struct {
	Provider     string            `toml:"provider"`
	Model        string            `toml:"model"`
	CLI          string            `toml:"cli"`
	BaseURL      string            `toml:"base_url"`
	APIKeyEnv    string            `toml:"api_key_env"`
	Temperature  *float64          `toml:"temperature"`
	MaxTokens    *int              `toml:"max_tokens"`
	TimeoutSec   *float64          `toml:"timeout_sec"`
	ExtraHeaders map[string]string `toml:"extra_headers"`
}

// Cadence is each loop's period in seconds.
type Cadence struct {
	TechnicalLoopSec      float64 `toml:"technical_loop_sec"`
	EventLoopSec          float64 `toml:"event_loop_sec"`
	NewsPollSec           float64 `toml:"news_poll_sec"`
	NewsSymbolThrottleSec float64 `toml:"news_symbol_throttle_sec"`
	ReconcilerPollSec     float64 `toml:"reconciler_poll_sec"`
	RiskMonitorSec        float64 `toml:"risk_monitor_sec"`
	ExecutorPollSec       float64 `toml:"executor_poll_sec"`
	ReflectCheckSec       float64 `toml:"reflect_check_sec"`
	InvariantsCheckSec    float64 `toml:"invariants_check_sec"`
	HeartbeatStaleSec     float64 `toml:"heartbeat_stale_sec"`
}

// defaultTrading carries the fallbacks the Python call sites passed to .get().
func defaultTrading() Trading {
	return Trading{
		Execution: Execution{EntryOrderTTLMin: 45},
		Cadence: Cadence{
			TechnicalLoopSec:      1800,
			NewsSymbolThrottleSec: 0.5,
			ReconcilerPollSec:     15,
			RiskMonitorSec:        10,
		},
	}
}

// Validate enforces the hard rules on trading.toml.
func (t *Trading) Validate() error {
	var errs []error
	if len(t.Universe.Symbols) == 0 {
		errs = append(errs, errors.New("universe.symbols is empty"))
	}
	for _, tier := range sortedKeys(t.Models) {
		m := t.Models[tier]
		if strings.TrimSpace(m.Provider) == "" {
			errs = append(errs, fmt.Errorf("models.%s.provider is empty", tier))
		}
		// Hard lesson「model 永不留空」.
		if strings.TrimSpace(m.Model) == "" {
			errs = append(errs, fmt.Errorf("models.%s.model is empty: pin it explicitly", tier))
		}
	}
	for _, tier := range []string{"trader", "flash"} {
		if _, ok := t.Models[tier]; !ok {
			errs = append(errs, fmt.Errorf("models.%s missing", tier))
		}
	}
	c := t.Cadence
	for name, v := range map[string]float64{
		"technical_loop_sec": c.TechnicalLoopSec, "event_loop_sec": c.EventLoopSec,
		"news_poll_sec": c.NewsPollSec, "reconciler_poll_sec": c.ReconcilerPollSec,
		"risk_monitor_sec": c.RiskMonitorSec, "executor_poll_sec": c.ExecutorPollSec,
		"reflect_check_sec": c.ReflectCheckSec, "invariants_check_sec": c.InvariantsCheckSec,
		"heartbeat_stale_sec": c.HeartbeatStaleSec,
	} {
		if v <= 0 {
			errs = append(errs, fmt.Errorf("cadence.%s = %v, must be > 0", name, v))
		}
	}
	if t.Execution.CommissionPerShare < 0 {
		errs = append(errs, errors.New("execution.commission_per_share < 0"))
	}
	return errors.Join(errs...)
}

// ── strategies.toml ──────────────────────────────────────────────────────

// Playbook binds a strategy's parameters to the symbols it applies to.
type Playbook struct {
	Name     string   `toml:"-"`
	Symbols  []string `toml:"symbols"`
	Binding  bool     `toml:"binding"`   // true: risk gate enforces the entry condition
	LongOnly bool     `toml:"long_only"` // never short under this playbook
	Lookback int      `toml:"lookback"`  // 4h bars in the confirmation window
	StopPct  float64  `toml:"stop_pct"`
	T1Pct    float64  `toml:"t1_pct"`
	T2Pct    float64  `toml:"t2_pct"`
}

// Strategies is strategies.toml, playbooks kept in file order.
type Strategies struct {
	Playbooks []Playbook
}

// PlaybookFor returns the playbook listing symbol, or nil.
func (s *Strategies) PlaybookFor(symbol string) *Playbook {
	for i := range s.Playbooks {
		if slices.Contains(s.Playbooks[i].Symbols, symbol) {
			return &s.Playbooks[i]
		}
	}
	return nil
}

// Validate rejects ambiguous or nonsensical playbooks.
func (s *Strategies) Validate() error {
	var errs []error
	owner := map[string]string{}
	for _, pb := range s.Playbooks {
		if len(pb.Symbols) == 0 {
			errs = append(errs, fmt.Errorf("playbooks.%s has no symbols", pb.Name))
		}
		for _, sym := range pb.Symbols {
			if prev, dup := owner[sym]; dup {
				errs = append(errs, fmt.Errorf("%s is bound to playbooks %s and %s", sym, prev, pb.Name))
			}
			owner[sym] = pb.Name
		}
		if pb.StopPct <= 0 || pb.T1Pct <= 0 || pb.T2Pct < pb.T1Pct {
			errs = append(errs, fmt.Errorf("playbooks.%s: need stop_pct > 0, t1_pct > 0, t2_pct >= t1_pct", pb.Name))
		}
		if pb.Lookback <= 0 {
			errs = append(errs, fmt.Errorf("playbooks.%s: lookback must be > 0", pb.Name))
		}
	}
	return errors.Join(errs...)
}

// ── loading ──────────────────────────────────────────────────────────────

// decodeStrict decodes data into v and fails on keys v does not declare.
func decodeStrict(name string, data []byte, v any) (toml.MetaData, error) {
	md, err := toml.Decode(string(data), v)
	if err != nil {
		return md, fmt.Errorf("%s: %w", name, err)
	}
	if und := md.Undecoded(); len(und) > 0 {
		keys := make([]string, len(und))
		for i, k := range und {
			keys[i] = k.String()
		}
		return md, fmt.Errorf("%s: unknown keys %s (typo or wrong table?)", name, strings.Join(keys, ", "))
	}
	return md, nil
}

// ParseRisk decodes and validates risk.toml content.
func ParseRisk(data []byte) (*Risk, error) {
	r := defaultRisk()
	if _, err := decodeStrict(RiskFile, data, &r); err != nil {
		return nil, err
	}
	if err := r.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", RiskFile, err)
	}
	return &r, nil
}

// ParseTrading decodes and validates trading.toml content.
func ParseTrading(data []byte) (*Trading, error) {
	t := defaultTrading()
	if _, err := decodeStrict(TradingFile, data, &t); err != nil {
		return nil, err
	}
	if err := t.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", TradingFile, err)
	}
	return &t, nil
}

// ParseStrategies decodes and validates strategies.toml content.
func ParseStrategies(data []byte) (*Strategies, error) {
	var raw struct {
		Playbooks map[string]Playbook `toml:"playbooks"`
	}
	md, err := decodeStrict(StrategiesFile, data, &raw)
	if err != nil {
		return nil, err
	}
	s := &Strategies{}
	seen := map[string]bool{}
	for _, k := range md.Keys() { // file order
		if len(k) == 2 && k[0] == "playbooks" && !seen[k[1]] {
			seen[k[1]] = true
			pb := raw.Playbooks[k[1]]
			pb.Name = k[1]
			s.Playbooks = append(s.Playbooks, pb)
		}
	}
	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", StrategiesFile, err)
	}
	return s, nil
}

func (l Loader) read(name string) ([]byte, error) {
	return os.ReadFile(filepath.Join(l.Dir, name))
}

// Risk reads risk.toml from disk.
func (l Loader) Risk() (*Risk, error) {
	b, err := l.read(RiskFile)
	if err != nil {
		return nil, err
	}
	return ParseRisk(b)
}

// Trading reads trading.toml from disk.
func (l Loader) Trading() (*Trading, error) {
	b, err := l.read(TradingFile)
	if err != nil {
		return nil, err
	}
	return ParseTrading(b)
}

// Strategies reads strategies.toml; a missing file means no playbooks
// (the system runs generic behaviour only), as in Python.
func (l Loader) Strategies() (*Strategies, error) {
	b, err := l.read(StrategiesFile)
	if errors.Is(err, fs.ErrNotExist) {
		return &Strategies{}, nil
	}
	if err != nil {
		return nil, err
	}
	return ParseStrategies(b)
}

// Parse validates content for the named config file without touching disk.
// The app's strategy editor runs this before it is allowed to save.
func Parse(name string, data []byte) error {
	var err error
	switch name {
	case RiskFile:
		_, err = ParseRisk(data)
	case TradingFile:
		_, err = ParseTrading(data)
	case StrategiesFile:
		_, err = ParseStrategies(data)
	default:
		err = fmt.Errorf("unknown config file %q", name)
	}
	return err
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
