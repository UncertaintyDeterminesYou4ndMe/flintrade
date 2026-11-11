package main

import (
	"context"
	"math"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"flintrade/cmd/flintrade-app/locales"
	"flintrade/internal/creds"
	"flintrade/internal/dash"
)

type credState struct {
	in         [3]string // new values typed in, by dash.CredKeys index; empty keeps the current one
	busy       bool
	err, msg   string
	askRestart bool
}

var credLabels = map[string]string{
	creds.AppKey:      "App Key",
	creds.AppSecret:   "App Secret",
	creds.AccessToken: "Access Token",
}

func (a *app) credsPage(c *ui.Context) {
	s, t := a.snap, c.Theme()
	cs := &a.cred
	ui.Scroll(c).Grow(1).Padding(pageInset...).Gap(20).Children(func() {
		sectionTitle(c, locales.CredsTitle())

		ui.Grid(c).Columns(2).Gap(20).AlignItems(ui.Start).Children(func() {
			card(c, func() {
				ui.Column(c).Gap(14).Padding(0, 0, 18, 0).Children(func() {
					heading(c, locales.CredsCurrent())
					field(c, locales.CredsFile(), s.CredPath)
					if err := creds.CheckPerms(s.CredPath); err != nil {
						field(c, locales.CredsPerms(), err.Error())
					}
					mode := locales.CredsModeDry()
					if s.Live {
						mode = locales.CredsModeLive()
					}
					field(c, locales.CredsMode(), locales.CredsModeNote(mode))
					if !s.Token.ExpiresAt.IsZero() {
						days := s.Token.ExpiresIn(time.Now()).Hours() / 24
						account := locales.CredsAccountLine(s.Token.Channel, s.Token.Region)
						if s.Token.Paper() {
							account = locales.CredsAccountLinePaper(s.Token.Channel, s.Token.Region)
						}
						field(c, locales.CredsAccount(), account)
						field(c, "Token", locales.CredsTokenDates(
							s.Token.IssuedAt.Local().Format("2006-01-02"), s.Token.ExpiresAt.Local().Format("2006-01-02"), int(math.Round(days))))
					}
					for _, k := range dash.CredKeys {
						masked := dash.Mask(s.CredValues[k])
						if masked == "" {
							masked = locales.CredsNotSet()
						}
						field(c, credLabels[k], masked)
					}
					for _, p := range s.TokenProblems {
						ui.Text(c, p.Message).TextColor(t.Danger)
					}
				})
			})

			card(c, func() {
				ui.Column(c).Gap(14).Padding(0, 0, 18, 0).Children(func() {
					heading(c, locales.CredsReplace())
					ui.Text(c, locales.CredsReplaceHint()).
						TextColor(t.TextMuted)
					for i, k := range dash.CredKeys {
						ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
							ui.Text(c, credLabels[k]).Width(110).TextColor(t.TextMuted)
							// Bound to a field that outlives the frame: the input writes it after this call returns.
							ui.TextInput(c.Key("cred-"+k), &cs.in[i]).Password().Label(credLabels[k]).Placeholder(locales.CredsKeepPlaceholder()).Grow(1).Disabled(cs.busy)
						})
					}
					ui.Column(c).Gap(8).Padding(6, 0, 0, 0).Children(func() {
						label := locales.CredsVerifySave()
						if cs.busy {
							label = locales.CredsVerifying()
						}
						ui.Row(c).Children(func() {
							if ui.PrimaryButton(c, label).Disabled(cs.busy).Clicked() {
								a.saveCreds()
							}
						})
						ui.Text(c, locales.CredsVerifyNote()).
							FontSize(12).TextColor(t.TextMuted)
					})
					if cs.err != "" {
						ui.Text(c, cs.err).TextColor(t.Danger).Selectable()
					}
					if cs.msg != "" {
						ui.Text(c, cs.msg)
					}
				})
			})
		})
	})
	switch ui.AlertDialog(c, &cs.askRestart, locales.CredsSavedTitle(),
		locales.CredsSavedBody(), locales.CredsLater(), locales.CommonRestart()) {
	case 1:
		a.restart()
	}
}

func (a *app) saveCreds() {
	cs := &a.cred
	edits := map[string]string{}
	for i, k := range dash.CredKeys {
		edits[k] = cs.in[i]
	}
	path, current := a.snap.CredPath, a.snap.CredValues
	cs.busy, cs.err, cs.msg = true, "", ""
	go func() {
		err := dash.SaveCreds(context.Background(), path, current, edits, time.Now())
		mygo.RunOnMain(func() {
			cs.busy = false
			if err != nil {
				cs.err = errText(err)
			} else {
				cs.in = [3]string{}
				cs.msg = locales.CredsWritten(path)
				cs.askRestart = true
				a.refreshNow()
			}
			if a.win != nil {
				a.win.Invalidate()
			}
		})
	}()
}
