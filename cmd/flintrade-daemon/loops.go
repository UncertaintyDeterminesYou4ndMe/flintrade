package main

import (
	"time"

	"flintrade/internal/executor"
	"flintrade/internal/invariants"
	"flintrade/internal/loop"
	"flintrade/internal/producers/event"
	"flintrade/internal/producers/news"
	"flintrade/internal/producers/technical"
	"flintrade/internal/reconciler"
	"flintrade/internal/reflect"
	"flintrade/internal/riskmonitor"
)

// buildLoops wires every loop with its cadence from trading.toml [cadence]
// (read once at start, as the Python daemon did; restart to apply changes).
func buildLoops(d *loop.Deps) ([]scheduled, error) {
	t, err := d.Config.Trading()
	if err != nil {
		return nil, err
	}
	sec := func(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }
	c := t.Cadence
	return []scheduled{
		{executor.New(d), sec(c.ExecutorPollSec)},
		{reconciler.New(d), sec(c.ReconcilerPollSec)},
		{riskmonitor.New(d), sec(c.RiskMonitorSec)},
		{technical.New(d), sec(c.TechnicalLoopSec)},
		{event.New(d), sec(c.EventLoopSec)},
		{news.New(d), sec(c.NewsPollSec)},
		{reflect.New(d), sec(c.ReflectCheckSec)},
		{invariants.New(d), sec(c.InvariantsCheckSec)},
	}, nil
}
