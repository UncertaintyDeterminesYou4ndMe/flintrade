package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"flintrade/cmd/flintrade-app/locales"
	"flintrade/internal/db"
)

const none = "—"

func commas(n int64) string {
	s := strconv.FormatInt(n, 10)
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// money: $9,140.92 (negative as −$23.76). Rounding comes first, so
// −0.004 is $0.00, not −$0.00.
func money(v float64) string {
	cents := int64(math.Round(v * 100))
	sign := ""
	if cents < 0 {
		sign, cents = "−", -cents
	}
	return fmt.Sprintf("%s$%s.%02d", sign, commas(cents/100), cents%100)
}

// signed: +$12.30 / −$23.76, and $0.00 without a sign: zero is neither.
func signed(v float64) string {
	if math.Round(v*100) > 0 {
		return "+" + money(v)
	}
	return money(v)
}

func moneyP(v *float64) string {
	if v == nil {
		return none
	}
	return money(*v)
}

func price(v *float64) string {
	if v == nil {
		return none
	}
	return strconv.FormatFloat(*v, 'f', 2, 64)
}

func intP(v *int) string {
	if v == nil {
		return none
	}
	return strconv.Itoa(*v)
}

func str(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

// localTime renders a stored UTC timestamp in the Mac's zone.
func localTime(ts string) string {
	t, err := db.ParseTime(ts)
	if err != nil {
		return ts
	}
	return t.Local().Format("01-02 15:04")
}

func ago(d time.Duration) string {
	d = max(d, 0) // a row stamped a moment after the snapshot
	switch {
	case d < time.Minute:
		return locales.AgoSeconds(int(d.Seconds()))
	case d < time.Hour:
		return locales.AgoMinutes(int(d.Minutes()))
	case d < 48*time.Hour:
		return locales.AgoHours(fmt.Sprintf("%.1f", d.Hours()))
	}
	return locales.AgoDays(fmt.Sprintf("%.1f", d.Hours()/24))
}

func every(d time.Duration) string {
	switch {
	case d == 0:
		return none
	case d < time.Minute:
		return locales.EverySeconds(int(d.Seconds()))
	case d < time.Hour:
		return locales.EveryMinutes(int(d.Minutes()))
	}
	return locales.EveryHours(fmt.Sprintf("%.1f", d.Hours()))
}
