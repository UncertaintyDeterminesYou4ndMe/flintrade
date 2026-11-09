package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/chart"

	"flintrade/internal/dash"
	"flintrade/internal/db"
)

var pages = [][2]string{{"overview", "总览"}, {"trades", "成交"}, {"intents", "意图"}, {"memory", "复盘"},
	{"strategy", "策略"}, {"creds", "凭证"}}

func (a *app) view(c *ui.Context) {
	t := useTheme(c)
	if a.rootErr != nil {
		ui.Column(c).Fill().Center().Gap(8).Padding(32).Children(func() {
			ui.Text(c, "找不到 Flintrade 仓库").FontSize(22)
			ui.Text(c, a.rootErr.Error()).TextColor(t.TextMuted).Selectable()
			ui.Text(c, "App 从 launchd 任务 com.flintrade.daemon 的 WorkingDirectory 定位仓库,或读环境变量 FLINTRADE_DIR。").TextColor(t.TextMuted)
		})
		return
	}
	if a.snap == nil {
		ui.Column(c).Fill().Center().Children(func() { ui.Text(c, "读取中").TextColor(t.TextMuted) })
		return
	}
	ui.Column(c).Fill().Children(func() {
		ui.Column(c).Padding(18, 32, 10, 32).Gap(14).Children(func() {
			a.header(c, a.snap, t)
			pills(c, &a.page, pages)
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
	switch ui.AlertDialog(c, &a.confirmRestart, "重启 daemon?",
		"launchd 会结束当前进程并立刻重新启动。挂单和持仓都在账本里,新进程接着跟踪。", "取消", "重启") {
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
		state, stateC := "daemon 运行中", t.Success
		switch {
		case s.DaemonDown():
			state, stateC = "daemon 已停止", t.Danger
		case s.Halted():
			state, stateC = "已暂停开新仓", t.Danger
		}
		ui.Row(c).Gap(8).AlignItems(ui.Center).Padding(7, 14).Radius(999).Background(t.Surface).Children(func() {
			dot(c, modeC)
			ui.Text(c, mode).FontSize(13)
			ui.Text(c, "·").FontSize(13).TextColor(t.TextMuted)
			dot(c, stateC)
			ui.Text(c, state).FontSize(13)
		})
		ui.Text(c, s.At.Format("15:04:05")).FontSize(12).FontFeatures("tnum").TextColor(t.TextMuted)
		label := "重启 daemon"
		if a.restarting {
			label = "重启中"
		}
		if ui.Button(c, label).Radius(999).Disabled(a.restarting).Clicked() {
			a.confirmRestart = true
		}
	})
	if a.notice != "" {
		ui.Text(c, a.notice).FontSize(12).TextColor(t.TextMuted)
	}
	if s.Err != nil {
		ui.Text(c, "账本读取不完整:"+s.Err.Error()).FontSize(12).TextColor(t.Danger).Selectable()
	}
	for _, p := range s.TokenProblems {
		ui.Text(c, "凭证:"+p.Message).FontSize(12).TextColor(t.Danger)
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
	ui.Row(c).AlignItems(ui.End).Gap(10).Children(func() {
		sectionTitle(c, "持仓")
		if s.Risk != nil {
			ui.Text(c, "在险 "+money(s.Risk.OpenRisk)).FontSize(13).TextColor(t.TextMuted).Padding(0, 0, 3, 0)
		}
	})
	if len(s.Positions) == 0 && len(s.Orders) == 0 {
		ui.Text(c, "空仓").TextColor(t.TextMuted).Padding(10, 0)
		ui.Divider(c)
	}
	for _, p := range s.Positions {
		sub := fmt.Sprintf("%s %d 股 · 止损 %s", sideZh(p.Side), p.Qty, price(p.Stop))
		r := recs[p.Symbol]
		row(c, "pos-"+p.Symbol, short(p.Symbol), sub, r.Series, 0,
			fmt.Sprintf("%.2f", p.EntryPrice), "风险 "+moneyP(p.RiskAmt), math.NaN())
	}
	for _, o := range s.Orders {
		sub := fmt.Sprintf("挂单 %s %d · 已成 %d", o.Side, o.Qty, o.FilledQty)
		row(c, "ord-"+o.ClientOrderID, short(o.Symbol), sub, nil, 0, price(o.Price), o.Status, math.NaN())
	}

	ui.Row(c).Height(18)
	ui.Row(c).AlignItems(ui.End).Gap(10).Children(func() {
		sectionTitle(c, "标的战绩")
		ui.Text(c, "策略平仓累计").FontSize(13).TextColor(t.TextMuted).Padding(0, 0, 3, 0)
	})
	for i, r := range s.Symbols {
		if i == 14 {
			break
		}
		sub := fmt.Sprintf("%d 笔 · 胜率 %.0f%%", r.N, float64(r.Wins)/float64(r.N)*100)
		avg := r.Net / float64(r.N)
		row(c, "rec-"+r.Symbol, short(r.Symbol), sub, r.Series, 0, signed(r.Net), "均 "+signed(avg), avg)
	}
}

func (a *app) cards(c *ui.Context, s *dash.Snapshot, t *ui.Theme) {
	ui.Row(c).Gap(12).AlignItems(ui.Stretch).Children(func() {
		// Equity against the inception sleeve.
		card(c, func() {
			ui.Text(c, "权益").FontSize(16)
			eq := none
			if s.Risk != nil && s.Risk.Equity != nil {
				eq = money(*s.Risk.Equity)
			}
			ui.Text(c, eq).FontSize(14).FontFeatures("tnum").TextColor(t.TextMuted)
			if r := s.Return(); r != nil {
				ui.Text(c, "("+signed(*r)+")").FontSize(14).FontFeatures("tnum").TextColor(t.TextMuted)
				change(c, pct(*r/s.InceptionEquity*100), *r, 20)
			}
			ys := []float64{s.InceptionEquity}
			for _, p := range s.Curve {
				ys = append(ys, p.Equity)
			}
			spark(c, tail(ys, 120), s.InceptionEquity).Height(64).Padding(0, 0, 0, 0)
		}).Grow(1)

		card(c, func() {
			ui.Text(c, "今日").FontSize(16)
			n := len(s.DaySeries)
			ui.Text(c, fmt.Sprintf("已平仓 %d 笔", n)).FontSize(14).TextColor(t.TextMuted)
			day := s.DayPnL()
			if day != nil {
				ui.Text(c, "("+signed(*day)+")").FontSize(14).FontFeatures("tnum").TextColor(t.TextMuted)
				base := *s.Risk.DayStartEquity
				change(c, pct(*day/base*100), *day, 20)
			}
			spark(c, append([]float64{0}, s.DaySeries...), 0).Height(64)
		}).Grow(1)

		card(c, func() {
			ui.Text(c, "胜率").FontSize(16)
			n, wins := 0, 0
			for _, r := range s.Symbols {
				n, wins = n+r.N, wins+r.Wins
			}
			ui.Text(c, fmt.Sprintf("策略平仓 %d 笔", n)).FontSize(14).TextColor(t.TextMuted)
			if len(s.WinRateSeries) > 0 {
				last := s.WinRateSeries[len(s.WinRateSeries)-1]
				ui.Text(c, fmt.Sprintf("(全部 %.1f%%)", float64(wins)/float64(max(n, 1))*100)).FontSize(14).TextColor(t.TextMuted)
				change(c, fmt.Sprintf("近 20 笔 %.0f%%", last*100), last-0.5, 20)
			}
			spark(c, tail(s.WinRateSeries, 120), 0.5).Height(64)
		}).Grow(1)
	})
}

var ranges = [][2]string{{"1w", "1 周"}, {"1m", "1 月"}, {"3m", "3 月"}, {"all", "全部"}}

func (a *app) equityChart(c *ui.Context, s *dash.Snapshot, t *ui.Theme) {
	ui.Column(c).Gap(12).Children(func() {
		ui.Row(c).AlignItems(ui.Center).Children(func() {
			sectionTitle(c, "权益走势")
			ui.Spacer(c)
			if a.rangeSel == "" {
				a.rangeSel = "all"
			}
			pills(c, &a.rangeSel, ranges)
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
			ui.Text(c, "这段时间平仓不足两笔").TextColor(t.TextMuted).Padding(40, 0)
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
		}).Height(260).Gap(8).Label("权益走势")
		ui.Text(c, "按策略平仓逐笔累计;虚线是区间起点。开仓佣金与 test / outage 归属的成交不计,所以终点与账面权益略有出入。").
			FontSize(12).TextColor(t.TextMuted)
	})
}

func (a *app) health(c *ui.Context, s *dash.Snapshot, t *ui.Theme) {
	ui.Column(c).Gap(10).Children(func() {
		sectionTitle(c, "运行状况")
		var stale []string
		for _, l := range s.Loops {
			if l.Stale && l.Core {
				stale = append(stale, l.Name)
			}
		}
		line := "交易链路正常:executor、reconciler、risk_monitor 都在按节奏心跳。"
		switch {
		case len(stale) > 0:
			line = "交易链路中断:" + strings.Join(stale, "、") + " 没有心跳。"
		case s.Halted():
			line = "风控已暂停开新仓:" + str(s.Risk.HaltReason) + "。平仓仍会执行。"
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
				last := "从未"
				if !l.LastBeat.IsZero() {
					last = ago(l.Age)
				}
				ui.Row(c.Key("loop-" + l.Name)).Gap(10).AlignItems(ui.Center).Children(func() {
					dot(c, col)
					ui.Text(c, l.Name).FontSize(14).Width(130)
					ui.Text(c, last).FontSize(13).TextColor(t.TextMuted)
					ui.Text(c, "每 "+every(l.Cadence)).FontSize(13).TextColor(t.TextMuted)
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
		items = append(items, feedItem{t: ts, who: in.Source, text: fmt.Sprintf("意图 #%d %s %s · %s", in.ID, sideZh(in.Side), short(in.Symbol), statusZh(in.Status))})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].t.After(items[j].t) })
	items = items[:min(len(items), 14)]

	ui.Column(c).Gap(10).Children(func() {
		ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
			sectionTitle(c, "最新动态")
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

func sideZh(s string) string {
	switch s {
	case "long":
		return "多"
	case "short":
		return "空"
	case "close":
		return "平"
	}
	return s
}

func statusZh(s string) string {
	return map[string]string{"pending": "待执行", "approved": "已批准", "rejected": "被拒", "filled": "已成交",
		"failed": "失败", "expired": "过期", "submitted": "已提交", "executed": "已执行"}[s] + orIf(s)
}

func orIf(s string) string {
	if _, ok := map[string]bool{"pending": true, "approved": true, "rejected": true, "filled": true, "failed": true,
		"expired": true, "submitted": true, "executed": true}[s]; ok {
		return ""
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
