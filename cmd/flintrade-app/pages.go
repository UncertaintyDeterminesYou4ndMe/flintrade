package main

import (
	"fmt"
	"strings"

	"github.com/egoist/mygo/ui"

	"flintrade/cmd/flintrade-app/locales"
	"flintrade/internal/producers/pj"
)

// master/detail: a virtualized table over a detail pane for the selected row.
func masterDetail(c *ui.Context, title, hint string, table func(), detail func()) {
	t := c.Theme()
	ui.Column(c).Grow(1).Padding(pageInset...).Gap(14).Children(func() {
		ui.Row(c).AlignItems(ui.End).Gap(12).Children(func() {
			sectionTitle(c, title)
			ui.Text(c, hint).FontSize(13).TextColor(t.TextMuted).Padding(0, 0, 3, 0)
		})
		tables(c, table)
		card(c, func() {
			ui.Scroll(c).Height(150).Gap(8).Padding(0, 0, 14, 0).Children(detail)
		})
	})
}

func field(c *ui.Context, label, value string) {
	if value == "" {
		return
	}
	t := c.Theme()
	ui.Row(c).Gap(12).AlignItems(ui.Start).Children(func() {
		ui.Text(c, label).Width(80).FontSize(12).TextColor(t.TextMuted)
		ui.Text(c, value).Grow(1).Selectable()
	})
}

func (a *app) memoryPage(c *ui.Context) {
	s, t := a.snap, c.Theme()
	ui.Column(c).Grow(1).Padding(pageInset...).Gap(16).Children(func() {
		ui.Row(c).AlignItems(ui.Center).Gap(16).Children(func() {
			sectionTitle(c, locales.MemoryTitle())
			ui.Segmented(c, &a.memTab, locales.MemoryTabLessons(), locales.MemoryTabReviews()).Width(240)
		})
		if a.memTab == 1 {
			a.reviewsView(c)
			return
		}
		ui.Scroll(c).Grow(1).Gap(24).Children(func() {
			if r := s.Recall; r != nil {
				sa := r.SelfAssessment
				ui.Grid(c).Columns(3).Gap(16).Children(func() {
					stat(c, locales.MemoryStatCloses(), locales.MemoryStatClosesValue(sa.N), locales.MemorySinceTrade(gapName(r.TimeAnchor.SinceLastTrade)), t.Text)
					wr := none
					if sa.WinRate != nil {
						wr = fmt.Sprintf("%.1f%%", float64(*sa.WinRate)*100)
					}
					stat(c, locales.MemoryStatWinRate(), wr, locales.MemorySinceDream(gapName(r.TimeAnchor.SinceLastDream)), t.Text)
					var net float64 // pj.F, or int 0 with no trade
					if f, ok := sa.NetPnL.(pj.F); ok {
						net = float64(f)
					}
					stat(c, locales.MemoryStatNet(), signed(net), locales.MemoryStatNetFoot(), pnlColor(t, net))
				})

				ui.Row(c).AlignItems(ui.End).Gap(12).Children(func() {
					sectionTitle(c, locales.MemoryLessons())
					ui.Text(c, locales.MemoryLessonsCount(len(r.Lessons))).FontSize(13).TextColor(t.TextMuted).Padding(0, 0, 3, 0)
				})
				if len(r.Lessons) == 0 {
					ui.Text(c, locales.MemoryLessonsEmpty()).TextColor(t.TextMuted)
				}
				ui.Column(c).Gap(0).Children(func() {
					for i, l := range r.Lessons {
						ui.Column(c.Key(fmt.Sprint("lesson-", i))).Children(func() {
							ui.Row(c).Gap(20).AlignItems(ui.Center).Padding(12, 0).Children(func() {
								confidence(c, float64(l.Confidence), l.N)
								ui.Text(c, l.Text).Grow(1).FontSize(14).Selectable()
							})
							ui.Divider(c)
						})
					}
				})
				if r.Plans != nil {
					ui.Row(c).AlignItems(ui.End).Gap(12).Children(func() {
						sectionTitle(c, locales.MemoryPlans())
						ui.Text(c, locales.MemoryPlansCount(len(*r.Plans))).FontSize(13).TextColor(t.TextMuted).Padding(0, 0, 3, 0)
					})
					for _, p := range *r.Plans {
						ui.Row(c).Gap(20).Padding(8, 0).Children(func() {
							ui.Text(c, locales.MemoryPlanExpires(orNone(str(p.ExpiresAt)))).Width(120).FontSize(13).TextColor(t.TextMuted)
							ui.Text(c, p.Text).Grow(1).Selectable()
						})
					}
				}
			}
			if st := s.ReviewStats; st != nil && len(st.ExitKindCounts) > 0 {
				sectionTitle(c, locales.MemoryExitKinds())
				var parts []string
				for k, n := range st.ExitKindCounts {
					parts = append(parts, fmt.Sprintf("%s %d", k, n))
				}
				ui.Row(c).Gap(10).Children(func() {
					for _, p := range sortStrings(parts) {
						ui.Text(c, p).FontSize(14).FontFeatures("tnum").Padding(8, 16).Radius(999).Background(t.Surface)
					}
				})
			}
		})
	})
}

// stat is a card with a label, a large figure and a muted footnote.
func stat(c *ui.Context, label, value, foot string, col ui.Color) {
	t := c.Theme()
	card(c, func() {
		ui.Text(c, label).FontSize(15)
		ui.Text(c, value).FontSize(30).FontFeatures("tnum").TextColor(col).Padding(4, 0)
		ui.Text(c, foot).FontSize(13).TextColor(t.TextMuted).Padding(0, 0, 16, 0)
	})
}

// confidence is a lesson's weight: the figure over a short bar, then n.
func confidence(c *ui.Context, v float64, n int) {
	t := c.Theme()
	ui.Column(c).Width(96).Gap(5).Children(func() {
		ui.Row(c).Gap(8).Children(func() {
			ui.Text(c, fmt.Sprintf("%.2f", v)).FontSize(14).FontFeatures("tnum")
			ui.Text(c, fmt.Sprintf("n=%d", n)).FontSize(12).FontFeatures("tnum").TextColor(t.TextMuted)
		})
		ui.Row(c).Height(4).Width(80).Draw(func(p *ui.Painter, r ui.Rect) {
			p.Fill(r, t.SurfacePressed, 2)
			p.Fill(ui.Rect{X: r.X, Y: r.Y, W: r.W * float32(max(0, min(1, v))), H: r.H}, t.Accent, 2)
		})
	})
}

func (a *app) reviewsView(c *ui.Context) {
	s, t := a.snap, c.Theme()
	rows := s.Reviews
	cols := []ui.TableColumn{
		{Title: locales.ReviewsColTime(), Width: 100}, {Title: locales.CommonSymbol(), Width: 100}, {Title: locales.CommonPnL(), Width: 100, Align: ui.End},
		{Title: locales.ReviewsColHeld(), Width: 110, Align: ui.End}, {Title: locales.ReviewsColExit(), Width: 90}, {Title: locales.ReviewsColVerdict(), Width: 90},
		{Title: locales.ReviewsColGrades(), Width: 130}, {Title: locales.CommonLesson()},
	}
	ui.Column(c).Grow(1).Gap(12).Children(func() {
		tables(c, func() {
			a.reviews.list.Key = func(i int) any { return rows[i].TradeID }
			ui.Table(c, &a.reviews.list, cols, len(rows), func(r, col int) {
				v := rows[r]
				switch col {
				case 0:
					ui.Text(c, localTime(v.TS)).SingleLine()
				case 1:
					ui.Text(c, v.Symbol).SingleLine()
				case 2:
					if v.PnL != nil {
						ui.Text(c, signed(*v.PnL)).FontFeatures("tnum").TextColor(pnlColor(t, *v.PnL))
					}
				case 3:
					ui.Text(c, intP(v.HoldMinutes)).FontFeatures("tnum")
				case 4:
					ui.Text(c, v.ExitKind).SingleLine()
				case 5:
					ui.Text(c, v.Verdict).SingleLine()
				case 6:
					ui.Text(c, v.EntryGrade+" / "+v.ExitGrade).SingleLine()
				case 7:
					ui.Text(c, v.Lesson).SingleLine().TextColor(t.TextMuted)
				}
			}).Grow(1)
		})
		ui.Divider(c)
		ui.Scroll(c).Height(170).Gap(6).Children(func() {
			i := a.reviews.sel
			if i < 0 || i >= len(rows) {
				ui.Text(c, locales.ReviewsSelectHint()).TextColor(t.TextMuted)
				return
			}
			v := rows[i]
			field(c, locales.CommonFill(), locales.ReviewsFill(fmt.Sprint(v.TradeID), v.Symbol, intP(v.Confidence)))
			field(c, locales.ReviewsThesis(), v.Thesis)
			field(c, locales.CommonLesson(), v.Lesson)
		})
	})
}

// gapName translates reflect.HumanGap ("3.2 hours", "never") for display.
func gapName(s string) string {
	switch s {
	case "just now":
		return locales.MemoryGapNow()
	case "never":
		return locales.MemoryGapNever()
	case "in the future":
		return locales.MemoryGapFuture()
	}
	if n, unit, ok := strings.Cut(s, " "); ok {
		switch unit {
		case "minutes":
			return locales.MemoryGapMinutes(n)
		case "hours":
			return locales.MemoryGapHours(n)
		case "days":
			return locales.MemoryGapDays(n)
		}
	}
	return s
}

func sortStrings(xs []string) []string {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j] < xs[j-1]; j-- {
			xs[j], xs[j-1] = xs[j-1], xs[j]
		}
	}
	return xs
}
