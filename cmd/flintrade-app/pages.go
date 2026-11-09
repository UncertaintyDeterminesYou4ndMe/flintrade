package main

import (
	"fmt"
	"strings"

	"github.com/egoist/mygo/ui"

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
	ui.Row(c).Gap(12).Children(func() {
		ui.Text(c, label).Width(80).FontSize(12).TextColor(t.TextMuted)
		ui.Text(c, value).Grow(1).Selectable()
	})
}

func (a *app) tradesPage(c *ui.Context) {
	s, t := a.snap, c.Theme()
	rows := s.Trades
	cols := []ui.TableColumn{
		{Title: "时间", Width: 100}, {Title: "动作", Width: 64}, {Title: "标的", Width: 100},
		{Title: "数量", Width: 60, Align: ui.End}, {Title: "成交价", Width: 90, Align: ui.End},
		{Title: "盈亏", Width: 100, Align: ui.End}, {Title: "来源", Width: 90}, {Title: "归属"},
	}
	masterDetail(c, "成交", fmt.Sprintf("最近 %d 笔,新的在上", len(rows)), func() {
		a.trades.list.Key = func(i int) any { return rows[i].ID }
		ui.Table(c, &a.trades.list, cols, len(rows), func(r, col int) {
			tr := rows[r]
			switch col {
			case 0:
				ui.Text(c, localTime(tr.TS)).SingleLine()
			case 1:
				ui.Text(c, tr.Action).SingleLine()
			case 2:
				ui.Text(c, tr.Symbol).SingleLine()
			case 3:
				ui.Text(c, fmt.Sprint(tr.Qty)).Font("monospace")
			case 4:
				ui.Text(c, fmt.Sprintf("%.2f", tr.FillPrice)).Font("monospace")
			case 5:
				if tr.PnL != nil {
					ui.Text(c, signed(*tr.PnL)).Font("monospace").TextColor(pnlColor(t, *tr.PnL))
				}
			case 6:
				ui.Text(c, str(tr.Source)).SingleLine()
			case 7:
				ui.Text(c, str(tr.Attribution)).SingleLine().TextColor(t.TextMuted)
			}
		}).Grow(1)
	}, func() {
		i := a.trades.sel
		if i < 0 || i >= len(rows) {
			ui.Text(c, "选中一笔成交查看理由").TextColor(t.TextMuted)
			return
		}
		tr := rows[i]
		field(c, "成交", fmt.Sprintf("#%d  %s %d %s @ %.2f  佣金 %s", tr.ID, tr.Action, tr.Qty, tr.Symbol, tr.FillPrice, money(tr.Commission)))
		if tr.IntentID != nil {
			field(c, "意图", fmt.Sprintf("#%d", *tr.IntentID))
		}
		field(c, "券商单号", str(tr.BrokerOrderID))
		field(c, "理由", str(tr.Reason))
	})
}

func (a *app) intentsPage(c *ui.Context) {
	s, t := a.snap, c.Theme()
	rows := s.Intents
	cols := []ui.TableColumn{
		{Title: "#", Width: 56, Align: ui.End}, {Title: "时间", Width: 100}, {Title: "来源", Width: 84},
		{Title: "标的", Width: 100}, {Title: "方向", Width: 60}, {Title: "置信度", Width: 60, Align: ui.End},
		{Title: "状态", Width: 90}, {Title: "理由"},
	}
	masterDetail(c, "意图", "生产者提出的假设,executor 过风控后决定做不做", func() {
		a.intents.list.Key = func(i int) any { return rows[i].ID }
		ui.Table(c, &a.intents.list, cols, len(rows), func(r, col int) {
			in := rows[r]
			switch col {
			case 0:
				ui.Text(c, fmt.Sprint(in.ID)).Font("monospace")
			case 1:
				ui.Text(c, localTime(in.CreatedAt)).SingleLine()
			case 2:
				ui.Text(c, in.Source).SingleLine()
			case 3:
				ui.Text(c, in.Symbol).SingleLine()
			case 4:
				ui.Text(c, in.Side).SingleLine()
			case 5:
				ui.Text(c, intP(in.Confidence)).Font("monospace")
			case 6:
				el := ui.Text(c, in.Status).SingleLine()
				if in.Status == "rejected" || in.Status == "failed" {
					el.TextColor(t.TextMuted)
				}
			case 7:
				ui.Text(c, firstLine(in.Reason)).SingleLine().TextColor(t.TextMuted)
			}
		}).Grow(1)
	}, func() {
		i := a.intents.sel
		if i < 0 || i >= len(rows) {
			ui.Text(c, "选中一条意图查看完整理由与拒绝原因").TextColor(t.TextMuted)
			return
		}
		in := rows[i]
		field(c, "意图", fmt.Sprintf("#%d  %s %s %s  %s", in.ID, in.Source, in.Side, in.Symbol, in.Status))
		field(c, "拒绝原因", in.RejectReason)
		field(c, "理由", in.Reason)
	})
}

func (a *app) memoryPage(c *ui.Context) {
	s, t := a.snap, c.Theme()
	ui.Column(c).Grow(1).Padding(pageInset...).Gap(16).Children(func() {
		ui.Row(c).AlignItems(ui.Center).Gap(16).Children(func() {
			sectionTitle(c, "复盘")
			ui.Segmented(c, &a.memTab, "教训与计划", "逐笔复盘").Width(240)
		})
		if a.memTab == 1 {
			a.reviewsView(c)
			return
		}
		ui.Scroll(c).Grow(1).Gap(24).Children(func() {
			if r := s.Recall; r != nil {
				sa := r.SelfAssessment
				ui.Grid(c).Columns(3).Gap(16).Children(func() {
					stat(c, "策略平仓", fmt.Sprintf("%d 笔", sa.N), "距上次成交 "+gapZh(r.TimeAnchor.SinceLastTrade), t.Text)
					wr := none
					if sa.WinRate != nil {
						wr = fmt.Sprintf("%.1f%%", float64(*sa.WinRate)*100)
					}
					stat(c, "胜率", wr, "距上次做梦 "+gapZh(r.TimeAnchor.SinceLastDream), t.Text)
					var net float64 // pj.F, or int 0 with no trade
					if f, ok := sa.NetPnL.(pj.F); ok {
						net = float64(f)
					}
					stat(c, "净盈亏", signed(net), "按策略平仓累计", pnlColor(t, net))
				})

				ui.Row(c).AlignItems(ui.End).Gap(12).Children(func() {
					sectionTitle(c, "教训")
					ui.Text(c, fmt.Sprintf("%d 条 · 左边是置信度和支撑样本数", len(r.Lessons))).FontSize(13).TextColor(t.TextMuted).Padding(0, 0, 3, 0)
				})
				if len(r.Lessons) == 0 {
					ui.Text(c, "还没有教训").TextColor(t.TextMuted)
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
						sectionTitle(c, "计划")
						ui.Text(c, fmt.Sprintf("%d 条", len(*r.Plans))).FontSize(13).TextColor(t.TextMuted).Padding(0, 0, 3, 0)
					})
					for _, p := range *r.Plans {
						ui.Row(c).Gap(20).Padding(8, 0).Children(func() {
							ui.Text(c, "到期 "+orNone(str(p.ExpiresAt))).Width(120).FontSize(13).TextColor(t.TextMuted)
							ui.Text(c, p.Text).Grow(1).Selectable()
						})
					}
				}
			}
			if st := s.ReviewStats; st != nil && len(st.ExitKindCounts) > 0 {
				sectionTitle(c, "出场方式")
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
		{Title: "时间", Width: 100}, {Title: "标的", Width: 100}, {Title: "盈亏", Width: 100, Align: ui.End},
		{Title: "持有(分)", Width: 80, Align: ui.End}, {Title: "出场", Width: 90}, {Title: "判断", Width: 90},
		{Title: "入/出场评分", Width: 90}, {Title: "教训"},
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
						ui.Text(c, signed(*v.PnL)).Font("monospace").TextColor(pnlColor(t, *v.PnL))
					}
				case 3:
					ui.Text(c, intP(v.HoldMinutes)).Font("monospace")
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
				ui.Text(c, "选中一笔复盘查看论点与教训").TextColor(t.TextMuted)
				return
			}
			v := rows[i]
			field(c, "成交", fmt.Sprintf("#%d %s  置信度 %s", v.TradeID, v.Symbol, intP(v.Confidence)))
			field(c, "论点", v.Thesis)
			field(c, "教训", v.Lesson)
		})
	})
}

// gapZh translates reflect.HumanGap ("3.2 hours", "never") for display.
func gapZh(s string) string {
	return strings.NewReplacer(" minutes", " 分钟", " hours", " 小时", " days", " 天", "just now", "刚刚", "never", "从未").Replace(s)
}

func sortStrings(xs []string) []string {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j] < xs[j-1]; j-- {
			xs[j], xs[j-1] = xs[j-1], xs[j]
		}
	}
	return xs
}
