package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

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

// money: $9,140.92 (negative as −$23.76).
func money(v float64) string {
	sign := ""
	if v < 0 {
		sign, v = "−", -v
	}
	cents := int64(math.Round(v * 100))
	return fmt.Sprintf("%s$%s.%02d", sign, commas(cents/100), cents%100)
}

// signed: +$12.30 / −$23.76.
func signed(v float64) string {
	if v > 0 || (v == 0 && !math.Signbit(v)) {
		return "+" + money(v)
	}
	return money(v)
}

// signedPlain: +12.30 / −23.76, for the menu bar.
func signedPlain(v float64) string {
	s := strings.TrimPrefix(strings.TrimPrefix(signed(v), "+"), "−")
	s = strings.TrimPrefix(s, "$")
	if v < 0 {
		return "−" + s
	}
	return "+" + s
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
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d 秒前", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d 分钟前", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%.1f 小时前", d.Hours())
	}
	return fmt.Sprintf("%.1f 天前", d.Hours()/24)
}

func every(d time.Duration) string {
	switch {
	case d == 0:
		return none
	case d < time.Minute:
		return fmt.Sprintf("%d 秒", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d 分钟", int(d.Minutes()))
	}
	return fmt.Sprintf("%.1f 小时", d.Hours())
}
