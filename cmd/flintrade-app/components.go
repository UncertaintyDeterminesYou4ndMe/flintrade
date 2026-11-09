package main

import (
	"fmt"
	"math"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/chart"
)

// arrow draws the round up/down chip after a change figure.
func arrow(c *ui.Context, v float64, size float32) {
	t := c.Theme()
	if v == 0 {
		return
	}
	col := pnlColor(t, v)
	up := v > 0
	ui.Row(c).Width(size).Height(size).Draw(func(p *ui.Painter, r ui.Rect) {
		cx, cy, rad := r.X+r.W/2, r.Y+r.H/2, r.W/2
		p.FillPath((&ui.Path{}).Circle(cx, cy, rad), col)
		// A solid head and a stem, which stay legible at 12 px.
		s := float32(1)
		if !up {
			s = -1
		}
		tip, neck, tail := cy-s*rad*0.58, cy-s*rad*0.02, cy+s*rad*0.58
		hw, sw := rad*0.5, rad*0.13
		p.FillPath((&ui.Path{}).MoveTo(cx, tip).LineTo(cx+hw, neck).LineTo(cx-hw, neck).Close(), t.Background)
		p.Fill(ui.Rect{X: cx - sw, Y: min(neck, tail), W: 2 * sw, H: abs32(tail - neck)}, t.Background, 0)
	})
}

// change is a coloured figure with its arrow chip: "+0.31% ↑".
func change(c *ui.Context, text string, v float64, size float32) {
	t := c.Theme()
	ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
		ui.Text(c, text).FontSize(size).FontFeatures("tnum").TextColor(pnlColor(t, v)).NoWrap()
		arrow(c, v, size+1)
	})
}

// spark draws a series as a line over a fading area, against a dotted
// baseline. It is green when the last value is at or above base, red
// below. With fewer than two points it leaves the space empty.
func spark(c *ui.Context, ys []float64, base float64) ui.Element {
	t := c.Theme()
	return ui.Row(c).Draw(func(p *ui.Painter, r ui.Rect) {
		if len(ys) < 2 {
			return
		}
		lo, hi := base, base
		for _, v := range ys {
			lo, hi = math.Min(lo, v), math.Max(hi, v)
		}
		if hi == lo {
			hi, lo = hi+1, lo-1
		}
		pad := (hi - lo) * 0.08
		lo, hi = lo-pad, hi+pad
		frac := func(v float64) float32 { return float32((v - lo) / (hi - lo)) }
		n := len(ys)
		xs, fs, zero := make([]float32, n), make([]float32, n), make([]float32, n)
		for i, v := range ys {
			xs[i], fs[i] = float32(i)/float32(n-1), frac(v)
		}
		col := t.Success
		if ys[n-1] < base {
			col = t.Danger
		}
		by := chart.Y(r, frac(base))
		p.FillPathGradient(chart.Area(r, xs, zero, fs, chart.CurveLinear),
			ui.LinearGradient{From: col.Alpha(0.32), To: col.Alpha(0), Angle: 180})
		p.StrokePath((&ui.Path{}).MoveTo(r.X, by).LineTo(r.X+r.W, by).Dash(0, 1.5, 3), 1, t.TextMuted.Alpha(0.7))
		p.StrokePath(chart.Line(r, xs, fs, chart.CurveLinear), 1.6, col)
	})
}

// card is a soft rounded panel.
func card(c *ui.Context, fn func()) ui.Element {
	t := c.Theme()
	return ui.Column(c).Background(t.Surface).Radius(16).Padding(16, 18, 0, 18).Gap(4).Children(fn)
}

// pills is the row of rounded tabs; the selected one is outlined.
func pills(c *ui.Context, sel *string, items [][2]string) {
	t := c.Theme()
	ui.Row(c).Gap(4).AlignItems(ui.Center).Children(func() {
		for _, it := range items {
			id, label := it[0], it[1]
			on := *sel == id
			el := ui.Row(c.Key("pill-"+id)).Padding(8, 20).Radius(999).Cursor(ui.CursorPointer).Label(label)
			if on {
				el = el.Border(1, t.TextMuted)
			}
			el = el.Children(func() {
				col := t.TextMuted
				if on {
					col = t.Text
				}
				ui.Text(c, label).FontSize(15).TextColor(col).NoWrap()
			})
			if el.Hovered() && !on {
				el.Background(t.SurfaceHover)
			}
			if el.Clicked() {
				*sel = id
			}
		}
	})
}

// sectionTitle is a large regular-weight heading.
func sectionTitle(c *ui.Context, title string) {
	ui.Text(c, title).FontSize(22)
}

// row is a watchlist line: name over a subtitle, a sparkline, a value
// over its change.
func row(c *ui.Context, key, name, sub string, ys []float64, base float64, value string, chg string, chgV float64) {
	t := c.Theme()
	ui.Column(c.Key(key)).Gap(0).Children(func() {
		ui.Row(c).Gap(12).AlignItems(ui.Center).Padding(10, 0).Children(func() {
			ui.Column(c).Width(120).Gap(2).Children(func() {
				ui.Text(c, name).FontSize(15).NoWrap()
				ui.Text(c, sub).FontSize(12).TextColor(t.TextMuted).NoWrap()
			})
			spark(c, ys, base).Width(96).Height(30)
			ui.Column(c).Grow(1).AlignItems(ui.End).Gap(2).Children(func() {
				ui.Text(c, value).FontSize(15).FontFeatures("tnum").NoWrap()
				if math.IsNaN(chgV) {
					ui.Text(c, chg).FontSize(12).TextColor(t.TextMuted).NoWrap()
				} else {
					change(c, chg, chgV, 12)
				}
			})
		})
		ui.Divider(c)
	})
}

func abs32(x float32) float32 {
	if x < 0 {
		return -x
	}
	return x
}

func pct(v float64) string {
	if v < 0 {
		return fmt.Sprintf("−%.2f%%", -v)
	}
	return fmt.Sprintf("+%.2f%%", v)
}

// pageInset matches the overview's margins on every page.
var pageInset = []float32{8, 32, 24, 32}
