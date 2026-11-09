package main

import (
	"context"
	"fmt"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

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
		sectionTitle(c, "凭证")

		ui.Grid(c).Columns(2).Gap(20).AlignItems(ui.Start).Children(func() {
			card(c, func() {
				ui.Column(c).Gap(14).Padding(0, 0, 18, 0).Children(func() {
					heading(c, "现在用的")
					field(c, "文件", s.CredPath)
					if err := creds.CheckPerms(s.CredPath); err != nil {
						field(c, "权限", err.Error())
					}
					mode := "DRY RUN:模拟成交,不下单"
					if s.Live {
						mode = "LIVE:在 paper 账户真实下单"
					}
					field(c, "模式", mode+"(由文件里的 FLINTRADE_DRY_RUN 决定,App 不提供开关)")
					if !s.Token.ExpiresAt.IsZero() {
						days := s.Token.ExpiresIn(time.Now()).Hours() / 24
						account := s.Token.Channel
						if s.Token.Paper() {
							account += "(模拟盘)"
						}
						field(c, "账户", account+" · "+s.Token.Region)
						field(c, "Token", fmt.Sprintf("签发 %s,到期 %s,还剩 %.0f 天",
							s.Token.IssuedAt.Local().Format("2006-01-02"), s.Token.ExpiresAt.Local().Format("2006-01-02"), days))
					}
					for _, k := range dash.CredKeys {
						field(c, credLabels[k], dash.Mask(s.CredValues[k]))
					}
					for _, p := range s.TokenProblems {
						ui.Text(c, p.Message).TextColor(t.Danger)
					}
				})
			})

			card(c, func() {
				ui.Column(c).Gap(14).Padding(0, 0, 18, 0).Children(func() {
					heading(c, "更换")
					ui.Text(c, "到 open.longbridge.com 开发者中心重新生成 Access Token(App Key / Secret 一般不变)。只填要换的,留空的保持原值。").
						TextColor(t.TextMuted)
					for i, k := range dash.CredKeys {
						ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
							ui.Text(c, credLabels[k]).Width(110).TextColor(t.TextMuted)
							// Bound to a field that outlives the frame: the input writes it after this call returns.
							ui.TextInput(c.Key("cred-"+k), &cs.in[i]).Password().Label(credLabels[k]).Placeholder("留空不改").Grow(1).Disabled(cs.busy)
						})
					}
					ui.Column(c).Gap(8).Padding(6, 0, 0, 0).Children(func() {
						label := "验证并保存"
						if cs.busy {
							label = "验证中"
						}
						ui.Row(c).Children(func() {
							if ui.PrimaryButton(c, label).Disabled(cs.busy).Clicked() {
								a.saveCreds()
							}
						})
						ui.Text(c, "先离线检查 token,再用新凭证调一次只读的 longbridge assets,通过才写入文件(原子写,0600)。").
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
	switch ui.AlertDialog(c, &cs.askRestart, "凭证已保存",
		"daemon 启动时才读凭证文件。现在重启 daemon 让新凭证生效?", "稍后", "重启") {
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
				cs.err = err.Error()
			} else {
				cs.in = [3]string{}
				cs.msg = "已写入 " + path
				cs.askRestart = true
				a.refreshNow()
			}
			if a.win != nil {
				a.win.Invalidate()
			}
		})
	}()
}
