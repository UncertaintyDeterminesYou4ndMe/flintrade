package main

import (
	"context"
	"image/png"
	"os"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"flintrade/internal/dash"
)

// Renders each page against the live ledger (read-only) to PNGs for a visual check.
//
//	FLINTRADE_SHOTS=<dir> go test ./cmd/flintrade-app -run Shots
func TestShots(t *testing.T) {
	dir := os.Getenv("FLINTRADE_SHOTS")
	if dir == "" {
		t.Skip("FLINTRADE_SHOTS not set")
	}
	a := newApp()
	if a.rootErr != nil {
		t.Fatal(a.rootErr)
	}
	a.snap = dash.Load(context.Background(), a.root, time.Now())
	defer func() { testLocale = "zh-Hans" }()
	for _, lang := range [][2]string{{"en", "en"}, {"zh-Hans", "zh"}} {
		testLocale = lang[0]
		for _, dark := range []bool{false, true} {
			for _, page := range []string{"overview", "trades", "intents", "memory", "strategy", "creds"} {
				a.page = page
				tt := ui.NewTesterWith(a.view, 1320, 880, ui.TesterOptions{Dark: dark})
				tt.SetScale(2) // Retina, as the Mac shows it
				if page == "trades" {
					tt.Click("UGL.US")
				}
				tt.Frame()
				name := page
				if dark {
					name += "-dark"
				}
				f, _ := os.Create(dir + "/" + name + "-" + lang[1] + ".png")
				png.Encode(f, tt.Image())
				f.Close()
			}
		}
	}
}
