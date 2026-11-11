package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/chart"

	"flintrade/cmd/flintrade-app/locales"
	"flintrade/internal/dash"
	"flintrade/internal/db"
	"flintrade/internal/marks"
)

func pageItems() [][2]string {
	return [][2]string{{"overview", locales.NavOverview()}, {"trades", locales.NavTrades()}, {"intents", locales.NavIntents()},
		{"memory", locales.NavMemory()}, {"strategy", locales.NavStrategy()}, {"creds", locales.NavCreds()}}
}

func (a *app) view(c *ui.Context) {
	t := useTheme(c)
	if a.rootErr != nil {
		ui.Column(c).Fill().Center().Gap(8).Padding(32).Children(func() {
			ui.Text(c, locales.StatusRepoMissing()).FontSize(22)
			ui.Text(c, a.rootErr.Error()).TextColor(t.TextMuted).Selectable()
			ui.Text(c, locales.ViewRepoHint()).TextColor(t.TextMuted)
		})
		return
	}
	if a.snap == nil {
		ui.Column(c).Fill().Center().Children(func() { ui.Text(c, locales.StatusLoading()).TextColor(t.TextMuted) })
		return
	}
	ui.Column(c).Fill().Children(func() {
		ui.Column(c).Padding(18, 32, 10, 32).Gap(14).Children(func() {
			a.header(c, a.snap, t)
			pills(c, &a.page, pageItems())
		})
		ui.Column(c).Grow(1).Children(func() {
			switch a.page {
			case "trades":
				a.tradesPage(c)
			case "intents":
				a.intentsPage(c)
			case "memory":
				a.memoryPage(c)
			case "strategy":
				a.strategyPage(c)
			case "creds":
				a.credsPage(c)
			default:
				a.overview(c)
			}
		})
	})
	switch ui.AlertDialog(c, &a.confirmRestart, locales.ViewRestartTitle(),
		locales.ViewRestartBody(), locales.CommonCancel(), locales.CommonRestart()) {
	case 1:
		a.restart()
	}
}

// dot is a small status light.
func dot(c *ui.Context, col ui.Color) {
	ui.Row(c).Width(8).Height(8).Radius(4).Background(col)
}

func (a *app) header(c *ui.Context, s *dash.Snapshot, t *ui.Theme) {
	ui.Row(c).Gap(14).AlignItems(ui.Center).Children(func() {
		ui.Row(c).Gap(8).AlignItems(ui.End).Children(func() {
			ui.Text(c, "Flintrade").FontSize(28)
			ui.Text(c, "paper trading").FontSize(17).TextColor(t.TextMuted).Padding(0, 0, 3, 0)
		})
		ui.Spacer(c)
		mode, modeC := "DRY RUN", t.TextMuted
		if s.Live {
			mode, modeC = "LIVE", t.Success
		}
		state, stateC := locales.StatusDaemonRunning(), t.Success
		switch {
		case s.DaemonDown():
			state, stateC = locales.StatusDaemonStopped(), t.Danger
		case s.Halted():
			state, stateC = locales.StatusEntriesPaused(), t.Danger
		}
		ui.Row(c).Gap(8).AlignItems(ui.Center).Padding(7, 14).Radius(999).Background(t.Surface).Children(func() {
			dot(c, modeC)
			ui.Text(c, mode).FontSize(13)
			ui.Text(c, "·").FontSize(13).TextColor(t.TextMuted)
			dot(c, stateC)
			ui.Text(c, state).FontSize(13)
		})
		ui.Text(c, s.At.Format("15:04:05")).FontSize(12).FontFeatures("tnum").TextColor(t.TextMuted)
		label := locales.CommonRestartDaemon()
		if a.restarting {
			label = locales.HeaderRestarting()
		}
		if ui.Button(c, label).Radius(999).Disabled(a.restarting).Clicked() {
			a.confirmRestart = true
		}
	})
	if a.notice != "" {
		ui.Text(c, a.notice).FontSize(12).TextColor(t.TextMuted)
	}
	if s.Err != nil {
		ui.Text(c, locales.HeaderLedgerError(s.Err.Error())).FontSize(12).TextColor(t.Danger).Selectable()
	}
	for _, p := range s.TokenProblems {
		ui.Text(c, locales.HeaderCredProblem(p.Key, p.Message)).FontSize(12).TextColor(t.Danger)
	}
}

// heading is a section title.
func heading(c *ui.Context, title string) {
	ui.Text(c, title).FontSize(18).Padding(6, 0, 2, 0)
}

func (a *app) overview(c *ui.Context) {
	s, t := a.snap, c.Theme()
	ui.Scroll(c).Grow(1).Padding(8, 32, 28, 32).Children(func() {
		ui.Row(c).Gap(28).AlignItems(ui.Start).Children(func() {
			ui.Column(c).Width(380).Gap(4).Children(func() { a.leftColumn(c, s, t) })
			ui.Row(c).Width(1).Height(900).Background(t.Border)
			ui.Column(c).Grow(1).Gap(28).Children(func() {
				a.cards(c, s, t)
				a.equityChart(c, s, t)
				a.health(c, s, t)
				a.feed(c, s, t)
			})
		})
	})
}

func (a *app) leftColumn(c *ui.Context, s *dash.Snapshot, t *ui.Theme) {
	recs := map[string]dash.SymbolRecord{}
	for _, r := range s.Symbols {
		recs[r.Symbol] = r
	}
	sectionTitle(c, locales.PositionTitle())
	if len(s.Positions) > 0 {
		ui.Column(c).Gap(2).Padding(0, 0, 4, 0).Children(func() {
			ui.Text(c, floatLabel(s)).FontSize(13).FontFeatures("tnum").TextColor(pnlColor(t, s.Float().Sum))
			ui.Text(c, allStopsLabel(s.Positions)).FontSize(13).FontFeatures("tnum").TextColor(t.TextMuted)
		})
	}
	if len(s.Positions) == 0 && len(s.Orders) == 0 {
		ui.Text(c, locales.PositionNone()).TextColor(t.TextMuted).Padding(10, 0)
		ui.Divider(c)
	}
	for _, p := range s.Positions {
		positionRow(c, s, p, recs[p.Symbol].Series)
	}
	for _, o := range s.Orders {
		sub := locales.PositionOrder(o.Side, fmt.Sprint(o.Qty), fmt.Sprint(o.FilledQty))
		row(c, "ord-"+o.ClientOrderID, short(o.Symbol), sub, nil, 0, price(o.Price), o.Status, math.NaN())
	}

	ui.Row(c).Height(18)
	ui.Row(c).AlignItems(ui.End).Gap(10).Children(func() {
		sectionTitle(c, locales.PositionRecords())
		ui.Text(c, locales.OverviewCumulative()).FontSize(13).TextColor(t.TextMuted).Padding(0, 0, 3, 0)
	})
	for i, r := range s.Symbols {
		if i == 14 {
			break
		}
		sub := locales.PositionRecordSub(r.N, fmt.Sprintf("%.0f", float64(r.Wins)/float64(r.N)*100))
		avg := r.Net / float64(r.N)
		row(c, "rec-"+r.Symbol, short(r.Symbol), sub, r.Series, 0, signed(r.Net), locales.PositionAvg(signed(avg)), avg)
	}
}

// floatLabel: "Unrealized P&L +$12.30"; "—" when no position has a price, so a missing
// price never reads as a flat zero.
func floatLabel(s *dash.Snapshot) string {
	f := s.Float()
	switch {
	case len(s.Marks) == 0:
		return locales.PositionFloatNoPrice()
	case !f.Complete:
		return locales.PositionFloatPartial(signed(f.Sum))
	}
	return locales.CommonFloat(signed(f.Sum))
}

// atStop is the P&L from entry if the position is stopped out at its current
// stop: negative is the loss the stop allows, positive a profit it locks in.
func atStop(p db.Position) (float64, bool) {
	if p.Stop == nil {
		return 0, false
	}
	return marks.PnL(p, *p.Stop), true
}

// allStopsLabel sums atStop over the positions: what every stop being hit
// would add up to.
func allStopsLabel(ps []db.Position) string {
	sum, bare := 0.0, false
	for _, p := range ps {
		if v, ok := atStop(p); ok {
			sum += v
		} else {
			bare = true
		}
	}
	if bare {
		return locales.PositionAllStopsBare(signed(sum))
	}
	return locales.PositionAllStops(signed(sum))
}

// positionRow: cost and stop on the left; on the right the last price the
// risk monitor saw, the floating P&L at it, and how old that price is.
func positionRow(c *ui.Context, s *dash.Snapshot, p db.Position, ys []float64) {
	t := c.Theme()
	m, priced := s.Marks[p.Symbol]
	ui.Column(c.Key("pos-" + p.Symbol)).Gap(0).Children(func() {
		ui.Row(c).Gap(12).AlignItems(ui.Center).Padding(10, 0).Children(func() {
			ui.Column(c).Width(120).Gap(2).Children(func() {
				ui.Text(c, short(p.Symbol)).FontSize(15).NoWrap()
				ui.Text(c, locales.PositionHeld(sideName(p.Side), fmt.Sprint(p.Qty), fmt.Sprintf("%.2f", p.EntryPrice))).FontSize(12).FontFeatures("tnum").TextColor(t.TextMuted).NoWrap()
				ui.Text(c, locales.PositionStop(price(p.Stop))).FontSize(12).FontFeatures("tnum").TextColor(t.TextMuted).NoWrap()
			})
			spark(c, ys, 0).Width(76).Height(30)
			ui.Column(c).Grow(1).AlignItems(ui.End).Gap(2).Children(func() {
				atStopV := none
				if v, ok := atStop(p); ok {
					atStopV = signed(v)
				}
				if !priced {
					ui.Text(c, none).FontSize(15).NoWrap()
					ui.Text(c, locales.PositionNoPrice()).FontSize(12).TextColor(t.TextMuted).NoWrap()
				} else {
					v := marks.PnL(p, m.Price)
					// The quote's age sits beside the price it dates; the
					// column is too narrow to add it to the line below.
					ui.Row(c).Gap(8).AlignItems(ui.Baseline).Children(func() {
						ui.Text(c, ago(s.At.Sub(m.Time()))).FontSize(12).FontFeatures("tnum").TextColor(t.TextMuted).NoWrap()
						ui.Text(c, fmt.Sprintf("%.2f", m.Price)).FontSize(15).FontFeatures("tnum").NoWrap()
					})
					change(c, signed(v)+" "+pct(v/(p.EntryPrice*float64(p.Qty))*100), v, 12)
				}
				ui.Text(c, locales.PositionAtStop(atStopV)).FontSize(12).FontFeatures("tnum").TextColor(t.TextMuted).NoWrap()
			})
		})
		ui.Divider(c)
	})
}

func (a *app) cards(c *ui.Context, s *dash.Snapshot, t *ui.Theme) {
	ui.Row(c).Gap(12).AlignItems(ui.Stretch).Children(func() {
		// Equity against the inception sleeve.
		card(c, func() {
			ui.Text(c, locales.OverviewNetEquity()).FontSize(16)
			eq := none
			if n := s.NetEquity(); n != nil {
				eq = money(*n)
				if r := s.Return(); r != nil {
					eq += " (" + signed(*r) + ")"
				}
			}
			ui.Text(c, eq).FontSize(14).FontFeatures("tnum").TextColor(t.TextMuted)
			if s.Risk != nil && s.Risk.Equity != nil {
				split := locales.OverviewRealized(money(*s.Risk.Equity))
				if len(s.Positions) > 0 {
					split = locales.OverviewRealizedFloat(money(*s.Risk.Equity), floatLabel(s))
				}
				ui.Text(c, split).FontSize(14).FontFeatures("tnum").TextColor(t.TextMuted).NoWrap()
			}
			if r := s.Return(); r != nil {
				change(c, pct(*r/s.InceptionEquity*100), *r, 20)
			}
			ys := []float64{s.InceptionEquity}
			for _, p := range s.Curve {
				ys = append(ys, p.Equity)
			}
			spark(c, tail(ys, 120), s.InceptionEquity).Height(64).Padding(0, 0, 0, 0)
		}).Grow(1)

		card(c, func() {
			ui.Text(c, locales.OverviewToday()).FontSize(16)
			n := len(s.DaySeries)
			day := s.Day()
			basis := "realized"
			if day != nil && day.WithFloat {
				basis = "float"
			}
			ui.Text(c, locales.OverviewClosedToday(n, basis)).FontSize(14).TextColor(t.TextMuted)
			if day != nil {
				ui.Text(c, "("+signed(day.PnL)+")").FontSize(14).FontFeatures("tnum").TextColor(t.TextMuted)
				change(c, pct(day.PnL/day.Base*100), day.PnL, 20)
			}
			spark(c, append([]float64{0}, s.DaySeries...), 0).Height(64)
		}).Grow(1)

		card(c, func() {
			ui.Text(c, locales.OverviewWinRate()).FontSize(16)
			n, wins := 0, 0
			for _, r := range s.Symbols {
				n, wins = n+r.N, wins+r.Wins
			}
			ui.Text(c, locales.OverviewStrategyCloses(n)).FontSize(14).TextColor(t.TextMuted)
			if len(s.WinRateSeries) > 0 {
				last := s.WinRateSeries[len(s.WinRateSeries)-1]
				ui.Text(c, locales.OverviewWinRateAll(fmt.Sprintf("%.1f", float64(wins)/float64(max(n, 1))*100))).FontSize(14).TextColor(t.TextMuted)
				change(c, locales.OverviewWinRateRecent(fmt.Sprintf("%.0f", last*100)), last-0.5, 20)
			}
			spark(c, tail(s.WinRateSeries, 120), 0.5).Height(64)
		}).Grow(1)
	})
}

func rangeItems() [][2]string {
	return [][2]string{{"1w", locales.OverviewRange1w()}, {"1m", locales.OverviewRange1m()}, {"3m", locales.OverviewRange3m()},
		{"all", locales.OverviewRangeAll()}}
}

func (a *app) equityChart(c *ui.Context, s *dash.Snapshot, t *ui.Theme) {
	ui.Column(c).Gap(12).Children(func() {
		ui.Row(c).AlignItems(ui.Center).Children(func() {
			sectionTitle(c, locales.OverviewCumulative())
			ui.Spacer(c)
			if a.rangeSel == "" {
				a.rangeSel = "all"
			}
			pills(c, &a.rangeSel, rangeItems())
		})
		pts := s.Curve
		since := map[string]time.Duration{"1w": 7 * 24 * time.Hour, "1m": 30 * 24 * time.Hour, "3m": 91 * 24 * time.Hour}[a.rangeSel]
		base := s.InceptionEquity
		if since > 0 {
			cut := s.At.Add(-since)
			i := sort.Search(len(pts), func(i int) bool { return pts[i].T.After(cut) })
			if i > 0 {
				base = pts[i-1].Equity
			}
			pts = pts[i:]
		}
		if len(pts) < 2 {
			ui.Text(c, locales.OverviewChartEmpty()).TextColor(t.TextMuted).Padding(40, 0)
			return
		}
		last := pts[len(pts)-1].Equity
		d := last - base
		ui.Row(c).Gap(14).AlignItems(ui.End).Children(func() {
			ui.Text(c, money(last)).FontSize(30).FontFeatures("tnum")
			change(c, fmt.Sprintf("%s  %s", signed(d), pct(d/base*100)), d, 16)
		})
		col := t.Success
		if d < 0 {
			col = t.Danger
		}
		vals := []float64{base}
		for _, p := range pts {
			vals = append(vals, p.Equity)
		}
		x := chart.NewTime(pts[0].T, pts[len(pts)-1].T)
		lo, hi := chart.Extent(vals...)
		y := chart.NewLinear(lo, hi).Nice(4)
		xs, ys, zero := make([]float32, len(pts)), make([]float32, len(pts)), make([]float32, len(pts))
		for i, p := range pts {
			xs[i], ys[i] = x.At(p.T), y.At(p.Equity)
		}
		by := y.At(base)
		yt, xt := y.Ticks(4), x.Ticks(6)
		label := func(_ chart.Side, tk chart.Tick) {
			ui.Text(c, tk.Label).NoWrap().FontSize(11).FontFeatures("tnum").TextColor(t.TextMuted)
		}
		chart.Plot(c, chart.Axes{Right: yt, Bottom: xt, Label: label}, func(area ui.Element) {
			area.Draw(func(p *ui.Painter, r ui.Rect) {
				for _, tk := range yt {
					p.Line(r.X, chart.Y(r, tk.At), r.X+r.W, chart.Y(r, tk.At), 1, t.Border)
				}
				p.FillPathGradient(chart.Area(r, xs, zero, ys, chart.CurveLinear),
					ui.LinearGradient{From: col.Alpha(0.28), To: col.Alpha(0), Angle: 180})
				p.StrokePath((&ui.Path{}).MoveTo(r.X, chart.Y(r, by)).LineTo(r.X+r.W, chart.Y(r, by)).Dash(0, 2, 4), 1, t.TextMuted.Alpha(0.8))
				p.StrokePath(chart.Line(r, xs, ys, chart.CurveLinear), 2, col)
			})
		}).Height(260).Gap(8).Label(locales.OverviewCumulative())
		ui.Text(c, locales.OverviewChartNote()).
			FontSize(12).TextColor(t.TextMuted)
	})
}

func (a *app) health(c *ui.Context, s *dash.Snapshot, t *ui.Theme) {
	ui.Column(c).Gap(10).Children(func() {
		sectionTitle(c, locales.OverviewHealth())
		var stale []string
		for _, l := range s.Loops {
			if l.Stale && l.Core {
				stale = append(stale, l.Name)
			}
		}
		line := locales.OverviewHealthOk()
		switch {
		case len(stale) > 0:
			line = locales.OverviewHealthDown(strings.Join(stale, locales.CommonListSeparator()))
		case s.Halted():
			line = locales.OverviewHealthHalted(str(s.Risk.HaltReason))
		}
		ui.Text(c, line).FontSize(16).LineHeight(1.5)
		ui.Grid(c).Columns(2).Gap(10).Children(func() {
			for _, l := range s.Loops {
				col := t.Success
				if l.Stale {
					col = t.TextMuted
					if l.Core {
						col = t.Danger
					}
				}
				last := locales.OverviewLoopNever()
				if !l.LastBeat.IsZero() {
					last = ago(l.Age)
				}
				ui.Row(c.Key("loop-" + l.Name)).Gap(10).AlignItems(ui.Center).Children(func() {
					dot(c, col)
					ui.Text(c, l.Name).FontSize(14).Width(130)
					ui.Text(c, last).FontSize(13).TextColor(t.TextMuted)
					ui.Text(c, locales.OverviewLoopEvery(every(l.Cadence))).FontSize(13).TextColor(t.TextMuted)
				})
			}
		})
	})
}

type feedItem struct {
	t     time.Time
	who   string
	text  string
	v     float64
	isPnL bool
}

func (a *app) feed(c *ui.Context, s *dash.Snapshot, t *ui.Theme) {
	var items []feedItem
	for _, tr := range s.Trades {
		ts, err := db.ParseTime(tr.TS)
		if err != nil {
			continue
		}
		it := feedItem{t: ts, who: str(tr.Source), text: fmt.Sprintf("%s %d %s @ %.2f", tr.Action, tr.Qty, short(tr.Symbol), tr.FillPrice)}
		if tr.PnL != nil {
			it.v, it.isPnL = *tr.PnL, true
		}
		items = append(items, it)
		if len(items) == 12 {
			break
		}
	}
	for i, in := range s.Intents {
		if i == 12 {
			break
		}
		ts, err := db.ParseTime(in.CreatedAt)
		if err != nil {
			continue
		}
		items = append(items, feedItem{t: ts, who: in.Source, text: locales.OverviewFeedIntent(fmt.Sprint(in.ID), sideName(in.Side), short(in.Symbol), statusName(in.Status))})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].t.After(items[j].t) })
	items = items[:min(len(items), 14)]

	ui.Column(c).Gap(10).Children(func() {
		ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
			sectionTitle(c, locales.OverviewFeed())
			if !s.DaemonDown() {
				ui.Text(c, "Live").FontSize(12).TextColor(t.Background).Background(t.Danger).Padding(1, 8).Radius(999)
			}
		})
		for i, it := range items {
			ui.Row(c.Key(fmt.Sprintf("feed-%d", i))).Gap(14).AlignItems(ui.Center).Children(func() {
				dot(c, t.TextMuted)
				ui.Text(c, ago(s.At.Sub(it.t))).FontSize(14).TextColor(t.TextMuted).Width(90).NoWrap()
				ui.Text(c, it.who).FontSize(14).Width(76).NoWrap()
				ui.Text(c, it.text).FontSize(15).Grow(1).NoWrap()
				if it.isPnL {
					change(c, signed(it.v), it.v, 14)
				}
			})
		}
	})
}

func tail(xs []float64, n int) []float64 {
	if len(xs) > n {
		return xs[len(xs)-n:]
	}
	return xs
}

// short drops the market suffix: GLD.US → GLD.
func short(sym string) string { return strings.TrimSuffix(sym, ".US") }

func sideName(s string) string {
	switch s {
	case "long":
		return locales.SideLong()
	case "short":
		return locales.SideShort()
	case "close":
		return locales.SideClose()
	}
	return s
}

// statusName names an intent or order status; an unknown one stays as it is.
func statusName(s string) string {
	switch s {
	case "pending":
		return locales.StatusPending()
	case "approved":
		return locales.StatusApproved()
	case "rejected":
		return locales.StatusRejected()
	case "filled":
		return locales.StatusFilled()
	case "failed":
		return locales.StatusFailed()
	case "expired":
		return locales.StatusExpired()
	case "cancelled":
		return locales.StatusCancelled()
	case "submitted":
		return locales.StatusSubmitted()
	case "executed":
		return locales.StatusExecuted()
	}
	return s
}

func orNone(s string) string {
	if s == "" {
		return none
	}
	return s
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}
