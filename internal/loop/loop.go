// Package loop defines what every daemon loop shares: its dependencies and
// the one-iteration contract the supervisor (cmd/flintrade-daemon) drives.
//
// Loops never call each other; they communicate only through flintrade.db, and
// each writes only what its db.Role permits.
package loop

import (
	"context"
	"time"

	"flintrade/internal/broker"
	"flintrade/internal/config"
	"flintrade/internal/db"
	"flintrade/internal/lb"
	"flintrade/internal/llm"
	"flintrade/internal/quotes"
	"flintrade/internal/session"
)

// Deps are goroutine-safe handles shared by all loops. A loop takes the
// role-scoped store it needs with DB.MustAs(role) — never a broader role.
type Deps struct {
	Root   string
	DB     *db.DB
	Config config.Loader // re-reads the toml on every call (hot reload)
	LB     *lb.Runner
	Broker broker.Broker
	Clock  *session.Clock
	LLM    *llm.Client
	Quotes quotes.Source
	Now    func() time.Time // nil → time.Now; tests pin it
}

// Time returns the loop clock.
func (d *Deps) Time() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// Loop is one cadence-driven unit.
//
// Name is the heartbeat process name and must match Python's
// (executor, reconciler, risk_monitor, loop_technical, loop_event,
// news_collector, reflect, invariants): the heartbeats table and every
// reader of it key on these strings.
//
// RunOnce does one iteration and returns human-readable log lines. It beats
// its own heartbeat. A returned error means "this iteration failed": the
// supervisor logs it and does not count it as progress for the watchdog.
type Loop interface {
	Name() string
	RunOnce(ctx context.Context) ([]string, error)
}
