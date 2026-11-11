package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/egoist/mygo/i18n"
	"github.com/egoist/mygo/ui"

	"flintrade/cmd/flintrade-app/locales"
	"flintrade/internal/dash"
	"flintrade/internal/reasoncode"
)

// intentDay heads the intents of one ET trading day.
type intentDay struct {
	Date time.Time
	N    int
	By   map[string]int // status → count
}

type intentItem struct {
	day *intentDay
	in  *dash.Intent
}

// intentItems groups intents (newest first) under a header per ET date.
func intentItems(ins []dash.Intent) []intentItem {
	var out []intentItem
	var cur *intentDay
	for i := range ins {
		in := &ins[i]
		d, ok := etDay(in.CreatedAt)
		if !ok {
			continue
		}
		if cur == nil || !cur.Date.Equal(d) {
			cur = &intentDay{Date: d, By: map[string]int{}}
			out = append(out, intentItem{day: cur})
		}
		cur.N++
		cur.By[in.Status]++
		out = append(out, intentItem{in: in})
	}
	return out
}

// statusOrder is the order the day summary lists statuses in.
var statusOrder = []string{"filled", "pending", "approved", "rejected", "expired", "cancelled", "failed"}

// settledAside: the intent ended without a fill.
func settledAside(s string) bool {
	return s == "rejected" || s == "expired" || s == "cancelled" || s == "failed"
}

func (d *intentDay) summary() string {
	parts := []string{locales.IntentsDayCount(d.N)}
	for _, s := range statusOrder {
		if n := d.By[s]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", statusName(s), n))
		}
	}
	return strings.Join(parts, " · ")
}

func intentSide(s string) string {
	switch s {
	case "long":
		return locales.IntentSideLong()
	case "short":
		return locales.IntentSideShort()
	case "close":
		return locales.IntentSideClose()
	case "flatten":
		return locales.IntentSideFlatten()
	}
	return s
}

// intentNote is what the list shows of an intent: why it ended without a
// fill when it did (the gate's or the broker's words), else the producer's
// reason.
func intentNote(in *dash.Intent) string {
	if in.RejectReason != "" && settledAside(in.Status) {
		return rejectText(in)
	}
	return firstLine(in.Reason)
}

// rejectText says why an intent ended without a fill in the app's language,
// from the code the daemon stored; an intent decided before codes existed
// (or with a key this build does not know) shows the daemon's text.
func rejectText(in *dash.Intent) string {
	c, ok := reasoncode.Parse(in.RejectCode)
	if !ok || !c.Valid() {
		return in.RejectReason
	}
	key := reasonKey(c.Key)
	if _, _, ok := locales.Catalog.Message(i18n.Locale(), key); !ok {
		return in.RejectReason
	}
	args := make(map[string]any, len(c.Args))
	for k, v := range c.Args {
		args[k] = v
	}
	return locales.Catalog.Format(i18n.Locale(), key, args)
}

// reasonKey is the catalog key of a reason code: "closeBlackout" →
// "reasonCloseBlackout".
func reasonKey(code string) string {
	if code == "" {
		return ""
	}
	return "reason" + strings.ToUpper(code[:1]) + code[1:]
}

// intentFills lists the fills an intent produced, among the recent ones.
func intentFills(s *dash.Snapshot, id int64) string {
	var out []string
	for _, tr := range s.Trades {
		if tr.IntentID != nil && *tr.IntentID == id {
			out = append(out, fmt.Sprintf("%s %s %d @ %.2f", localTime(tr.TS), actionName(tr.Action), tr.Qty, tr.FillPrice))
		}
	}
	return strings.Join(out, locales.CommonFillSeparator())
}

func (a *app) intentsPage(c *ui.Context) {
	s, t := a.snap, c.Theme()
	items := intentItems(s.Intents)
	cols := []ui.TableColumn{
		{Title: "#", Width: 56, Align: ui.End}, {Title: locales.CommonLocalTime(), Width: 100}, {Title: locales.CommonSource(), Width: 100},
		{Title: locales.CommonSymbol(), Width: 72}, {Title: locales.IntentsColSide(), Width: 64}, {Title: locales.CommonConfidence(), Width: 110, Align: ui.End},
		{Title: locales.IntentsColStatus(), Width: 96}, {Title: locales.IntentsColReason()},
	}
	hint := locales.IntentsHint()
	masterDetail(c, locales.IntentsTitle(), hint, func() {
		a.intents.list.Key = func(i int) any {
			if d := items[i].day; d != nil {
				return "day-" + d.Date.Format(time.DateOnly)
			}
			return items[i].in.ID
		}
		a.intents.list.Header = func(i int) bool { return items[i].day != nil }
		ui.Table(c, &a.intents.list, cols, len(items), func(r, col int) {
			if d := items[r].day; d != nil {
				dayHeader(c, d.Date, d.summary(), t.TextMuted)
				return
			}
			in := items[r].in
			aside := settledAside(in.Status)
			switch col {
			case 0:
				ui.Text(c, fmt.Sprint(in.ID)).FontFeatures("tnum").TextColor(t.TextMuted)
			case 1:
				ui.Text(c, localTime(in.CreatedAt)).FontFeatures("tnum").SingleLine()
			case 2:
				ui.Text(c, sourceName(in.Source)).SingleLine()
			case 3:
				ui.Text(c, short(in.Symbol)).SingleLine()
			case 4:
				ui.Text(c, intentSide(in.Side)).SingleLine()
			case 5:
				ui.Text(c, intP(in.Confidence)).FontFeatures("tnum")
			case 6:
				el := ui.Text(c, statusName(in.Status)).SingleLine()
				if aside {
					el.TextColor(t.TextMuted)
				}
			case 7:
				ui.Text(c, intentNote(in)).SingleLine().TextColor(t.TextMuted)
			}
		}).Grow(1)
	}, func() {
		i := a.intents.sel
		if i < 0 || i >= len(items) || items[i].in == nil {
			ui.Text(c, locales.IntentsSelectHint()).TextColor(t.TextMuted)
			return
		}
		in := items[i].in
		id, src, side, sym, status := fmt.Sprint(in.ID), sourceName(in.Source), intentSide(in.Side), short(in.Symbol), statusName(in.Status)
		head := locales.IntentsHead(id, src, side, sym, status)
		if in.Confidence != nil {
			head = locales.IntentsHeadConfidence(id, src, side, sym, status, fmt.Sprint(*in.Confidence))
		}
		field(c, locales.CommonIntent(), head)
		field(c, locales.CommonFill(), intentFills(s, in.ID))
		if settledAside(in.Status) {
			field(c, locales.IntentsNotFilled(), rejectText(in))
		} else {
			field(c, locales.IntentsNote(), rejectText(in))
		}
		field(c, locales.CommonReason(), in.Reason)
	})
}
