package main

import (
	"fmt"
	"time"

	"github.com/egoist/mygo/i18n"
	"github.com/egoist/mygo/ui"

	"flintrade/cmd/flintrade-app/locales"
	"flintrade/internal/dash"
	"flintrade/internal/db"
	"flintrade/internal/marks"
	"flintrade/internal/session"
)

// etDay is midnight ET of the trading day a UTC timestamp falls in: the
// day "today" uses, which the risk monitor rolls at ET midnight.
func etDay(ts string) (time.Time, bool) {
	t, err := db.ParseTime(ts)
	if err != nil {
		return time.Time{}, false
	}
	et := t.In(session.ET)
	return time.Date(et.Year(), et.Month(), et.Day(), 0, 0, 0, 0, session.ET), true
}

// dayTitle: "ET Mon, Nov 9" in English; the date follows the app language.
func dayTitle(d time.Time) string {
	return locales.CommonDayTitle(i18n.FormatDate(i18n.Locale(), d, i18n.DateOptions{Skeleton: "MMMdEEE"}))
}

// daySpan is the day on the Mac's own clock, which the time columns use. On
// a Beijing clock an ET day runs 13:00 to 13:00 (12:00 in US summer).
func daySpan(d time.Time) string {
	from, to := d.In(time.Local), d.AddDate(0, 0, 1).In(time.Local)
	return locales.CommonDaySpan(from.Format("01-02 15:04"), to.Format("01-02 15:04"))
}

// dayHeader is a section row: the ET date and its local span on the left,
// the day's summary on the right.
func dayHeader(c *ui.Context, d time.Time, summary string, col ui.Color) {
	t := c.Theme()
	ui.Row(c).Grow(1).AlignItems(ui.Center).Children(func() {
		ui.Text(c, dayTitle(d)).SingleLine()
		ui.Text(c, daySpan(d)).FontSize(12).FontWeight(400).FontFeatures("tnum").TextColor(t.TextMuted).SingleLine().Padding(0, 0, 0, 12)
		ui.Spacer(c)
		ui.Text(c, summary).FontFeatures("tnum").TextColor(col).SingleLine()
	})
}

// tradeDay heads the fills of one ET trading day.
type tradeDay struct {
	Date   time.Time // midnight ET
	Closed int       // fills that booked a P&L
	Wins   int
	PnL    float64
}

// tradeItem is one table row: a day header or a fill.
type tradeItem struct {
	day *tradeDay
	tr  *db.Trade
}

// tradeItems groups fills (newest first) under a header per ET date.
func tradeItems(trs []db.Trade) []tradeItem {
	var out []tradeItem
	var cur *tradeDay
	for i := range trs {
		tr := &trs[i]
		d, ok := etDay(tr.TS)
		if !ok {
			continue
		}
		if cur == nil || !cur.Date.Equal(d) {
			cur = &tradeDay{Date: d}
			out = append(out, tradeItem{day: cur})
		}
		if tr.PnL != nil {
			cur.Closed++
			cur.PnL += *tr.PnL
			if *tr.PnL > 0 {
				cur.Wins++
			}
		}
		out = append(out, tradeItem{tr: tr})
	}
	return out
}

func (d *tradeDay) summary() string {
	if d.Closed == 0 {
		return locales.TradesNoCloses()
	}
	return locales.TradesDaySummary(d.Closed, d.Wins, d.Closed-d.Wins, signed(d.PnL))
}

// actionName names a fill by what it does to the position.
func actionName(a string) string {
	switch a {
	case "BUY":
		return locales.TradesActionBuy()
	case "SELL":
		return locales.TradesActionSell()
	case "SHORT":
		return locales.TradesActionShort()
	case "COVER":
		return locales.TradesActionCover()
	}
	return a
}

func opening(a string) bool { return a == "BUY" || a == "SHORT" }

func sourceName(s string) string {
	switch s {
	case "technical":
		return locales.SourceTechnical()
	case "event":
		return locales.SourceEvent()
	case "user":
		return locales.SourceUser()
	case "risk_monitor":
		return locales.SourceRiskMonitor()
	}
	return s
}

// attributionName tags a fill that is not the strategy's own record.
func attributionName(a string) string {
	switch a {
	case "legacy":
		return locales.AttributionLegacy()
	case "test":
		return locales.AttributionTest()
	case "manual":
		return locales.AttributionManual()
	case "outage-degraded":
		return locales.AttributionOutage()
	}
	return a
}

func sourceLabel(tr *db.Trade) string {
	src := sourceName(str(tr.Source))
	if tr.Attribution != nil && *tr.Attribution != "strategy" {
		tag := attributionName(*tr.Attribution)
		if src == "" {
			return tag
		}
		src += " · " + tag
	}
	return src
}

// holding: for each opening fill of a still-open position, whether it is the
// newest one (which shows the position's floating P&L; older fills of the
// same position just say it is still held).
func holding(s *dash.Snapshot) map[int64]bool {
	open := map[int64]bool{}
	for _, p := range s.Positions {
		open[p.ID] = true
	}
	out, seen := map[int64]bool{}, map[int64]bool{}
	for _, tr := range s.Trades { // newest first
		if tr.PositionID == nil || !open[*tr.PositionID] || !opening(tr.Action) {
			continue
		}
		out[tr.ID] = !seen[*tr.PositionID]
		seen[*tr.PositionID] = true
	}
	return out
}

// positionFloat is a position's floating P&L at its last mark.
func positionFloat(s *dash.Snapshot, id int64) (float64, bool) {
	for _, p := range s.Positions {
		if p.ID == id {
			if m, ok := s.Marks[p.Symbol]; ok {
				return marks.PnL(p, m.Price), true
			}
		}
	}
	return 0, false
}

func (a *app) tradesPage(c *ui.Context) {
	s, t := a.snap, c.Theme()
	items := tradeItems(s.Trades)
	held := holding(s)
	cols := []ui.TableColumn{
		{Title: locales.CommonLocalTime(), Width: 100}, {Title: locales.TradesColAction(), Width: 96}, {Title: locales.CommonSymbol(), Width: 72},
		{Title: locales.TradesColQty(), Width: 56, Align: ui.End}, {Title: locales.TradesColPrice(), Width: 84, Align: ui.End},
		{Title: locales.CommonPnL(), Width: 240, Align: ui.End}, {Title: locales.CommonSource()},
	}
	hint := locales.TradesHint(len(s.Trades))
	masterDetail(c, locales.TradesTitle(), hint, func() {
		a.trades.list.Key = func(i int) any {
			if d := items[i].day; d != nil {
				return "day-" + d.Date.Format(time.DateOnly)
			}
			return items[i].tr.ID
		}
		a.trades.list.Header = func(i int) bool { return items[i].day != nil }
		ui.Table(c, &a.trades.list, cols, len(items), func(r, col int) {
			if d := items[r].day; d != nil {
				col := t.TextMuted
				if d.Closed > 0 {
					col = pnlColor(t, d.PnL)
				}
				dayHeader(c, d.Date, d.summary(), col)
				return
			}
			tr := items[r].tr
			switch col {
			case 0:
				ui.Text(c, localTime(tr.TS)).FontFeatures("tnum").SingleLine()
			case 1:
				ui.Text(c, actionName(tr.Action)).SingleLine()
			case 2:
				ui.Text(c, short(tr.Symbol)).SingleLine()
			case 3:
				ui.Text(c, fmt.Sprint(tr.Qty)).FontFeatures("tnum")
			case 4:
				ui.Text(c, fmt.Sprintf("%.2f", tr.FillPrice)).FontFeatures("tnum")
			case 5:
				switch newest, ok := held[tr.ID]; {
				case tr.PnL != nil:
					ui.Text(c, signed(*tr.PnL)).FontFeatures("tnum").TextColor(pnlColor(t, *tr.PnL))
				case ok:
					ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
						ui.Text(c, locales.TradesHeld()).TextColor(t.TextMuted).SingleLine()
						if v, priced := positionFloat(s, *tr.PositionID); newest && priced {
							ui.Text(c, locales.CommonFloat(signed(v))).FontFeatures("tnum").TextColor(pnlColor(t, v)).SingleLine()
						}
					})
				}
			case 6:
				ui.Text(c, sourceLabel(tr)).SingleLine().TextColor(t.TextMuted)
			}
		}).Grow(1)
	}, func() {
		i := a.trades.sel
		if i < 0 || i >= len(items) || items[i].tr == nil {
			ui.Text(c, locales.TradesSelectHint()).TextColor(t.TextMuted)
			return
		}
		tr := items[i].tr
		field(c, locales.CommonFill(), locales.TradesDetailFill(fmt.Sprint(tr.ID), actionName(tr.Action), fmt.Sprint(tr.Qty), short(tr.Symbol),
			fmt.Sprintf("%.2f", tr.FillPrice), money(tr.Commission)))
		if tr.IntentID != nil {
			field(c, locales.CommonIntent(), fmt.Sprintf("#%d", *tr.IntentID))
		}
		field(c, locales.TradesBrokerOrder(), str(tr.BrokerOrderID))
		field(c, locales.CommonReason(), str(tr.Reason))
	})
}
