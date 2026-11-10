// Command flintrade-app is Flintrade.app: a menu bar status item plus a window to
// read the ledger, edit strategy files and replace Longbridge credentials.
//
// It reads flintrade.db read-only and never orders. Its only writes are config
// and prompt files, the credentials file, and `launchctl kickstart` to make
// the daemon pick them up. Quitting the app does not stop trading.
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"flintrade/internal/dash"
)

const refreshEvery = 5 * time.Second

type tableState struct {
	list ui.ListState
	sel  int
}

type app struct {
	root    string
	rootErr error
	snap    *dash.Snapshot // latest; main thread only
	page    string

	win     *mygo.Window
	tray    *mygo.Tray
	refresh chan struct{}

	trades, intents, reviews tableState
	memTab                   int
	rangeSel                 string

	strat strategyState
	cred  credState

	confirmRestart bool
	restarting     bool
	notice         string // result of the last daemon action
}

func newApp() *app {
	a := &app{page: "overview", refresh: make(chan struct{}, 1)}
	for _, ts := range []*tableState{&a.trades, &a.intents, &a.reviews} {
		ts.sel = -1
		ts.list.Selected = &ts.sel
	}
	a.root, a.rootErr = dash.FindRoot()
	return a
}

func main() {
	a := newApp()
	mygo.App.WhenReady(func() {
		mygo.App.OnWindowAllClosed(func() {}) // live in the menu bar
		mygo.App.OnActivate(func(visible bool) {
			if !visible {
				a.open()
			}
		})
		tray, err := mygo.NewTray(mygo.TrayOptions{ID: "flintrade", Title: "Flintrade", ToolTip: "Flintrade", Menu: a.trayMenu()})
		if err != nil {
			log.Printf("tray: %v", err)
		}
		a.tray = tray
		a.open()
		go a.poll()
	})
	if err := mygo.App.Run(); err != nil {
		log.Fatal(err)
	}
}

func (a *app) open() {
	if a.win != nil && !a.win.IsDestroyed() {
		if a.win.IsMinimized() {
			a.win.Restore()
		}
		a.win.Show()
		a.win.Focus()
		mygo.App.Focus()
		return
	}
	mygo.App.SetActivationPolicy(mygo.ActivationPolicyRegular)
	a.win = mygo.NewWindow(mygo.WindowOptions{
		Title: "Flintrade", Width: 1320, Height: 880, MinWidth: 1040, MinHeight: 640,
		StateKey: "main", Content: ui.View(a.view),
	})
	a.win.OnClosed(func() {
		a.win = nil
		mygo.App.SetActivationPolicy(mygo.ActivationPolicyAccessory)
	})
	mygo.App.Focus()
}

// poll reloads the snapshot off the main thread and hands it over.
func (a *app) poll() {
	if a.rootErr != nil {
		mygo.RunOnMain(func() { a.applyTray() })
		return
	}
	tick := time.NewTicker(refreshEvery)
	defer tick.Stop()
	for {
		s := dash.Load(context.Background(), a.root, time.Now())
		mygo.RunOnMain(func() { a.apply(s) })
		select {
		case <-tick.C:
		case <-a.refresh:
		}
	}
}

func (a *app) refreshNow() {
	select {
	case a.refresh <- struct{}{}:
	default:
	}
}

func (a *app) apply(s *dash.Snapshot) {
	alerts := dash.Alerts(a.snap, s)
	a.snap = s
	a.applyTray()
	if a.win != nil {
		a.win.Invalidate()
	}
	if len(alerts) > 0 && mygo.NotificationsSupported() {
		go func() {
			for _, al := range alerts {
				if err := mygo.NewNotification(mygo.NotificationOptions{ID: "flintrade-" + al.Key, Title: al.Title, Body: al.Body}).Show(); err != nil {
					log.Printf("notify %s: %v", al.Key, err)
				}
			}
		}()
	}
}

// trayTitle is what the menu bar shows: today's PnL, or what is wrong.
func (a *app) trayTitle() string {
	s := a.snap
	switch {
	case a.rootErr != nil:
		return "Flintrade ?"
	case s == nil:
		return "Flintrade"
	case s.Err != nil && len(s.Loops) == 0:
		return "Flintrade ?"
	case s.DaemonDown():
		return "Flintrade 停"
	case s.Halted():
		return "Flintrade 暂停"
	}
	if p := s.DayPnL(); p != nil {
		return "Flintrade " + signedPlain(*p)
	}
	return "Flintrade"
}

func (a *app) statusLine() string {
	s := a.snap
	if a.rootErr != nil {
		return "找不到 Flintrade 仓库"
	}
	if s == nil {
		return "读取中"
	}
	mode := "DRY RUN"
	if s.Live {
		mode = "LIVE paper"
	}
	daemon := "daemon 运行中"
	if s.DaemonDown() {
		daemon = "daemon 已停止"
	} else if s.Halted() {
		daemon = "已暂停开新仓"
	}
	eq := "—"
	if s.Risk != nil && s.Risk.Equity != nil {
		eq = money(*s.Risk.Equity)
	}
	return fmt.Sprintf("%s · %s · 权益 %s", mode, daemon, eq)
}

// dayRollNote says when "today" restarts: the risk monitor rolls the day at
// midnight New York time, which is 13:00 (12:00 in US summer) in Beijing.
func dayRollNote(now time.Time) string {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		return "今日盈亏按美东日期计算"
	}
	n := now.In(ny)
	midnight := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, ny).In(now.Location())
	return "今日盈亏按美东日期,每天本地 " + midnight.Format("15:04") + " 换日"
}

func (a *app) trayMenu() *mygo.Menu {
	return mygo.NewMenu([]*mygo.MenuItem{
		{ID: "status", Label: a.statusLine(), Disabled: true},
		{ID: "day", Label: dayRollNote(time.Now()), Disabled: true},
		mygo.Separator(),
		{ID: "open", Label: "打开 Flintrade", Click: func(*mygo.MenuItem, *mygo.Window) { a.open() }},
		{ID: "restart", Label: "重启 daemon", Click: func(*mygo.MenuItem, *mygo.Window) { a.restart() }},
		mygo.Separator(),
		{ID: "quit", Label: "退出 Flintrade.app(交易照常)", Accelerator: "CmdOrCtrl+Q", Click: func(*mygo.MenuItem, *mygo.Window) { mygo.App.Quit() }},
	})
}

func (a *app) applyTray() {
	if a.tray == nil {
		return
	}
	a.tray.SetTitle(a.trayTitle())
	a.tray.SetMenu(a.trayMenu())
}

// restart kicks the daemon off the main thread.
func (a *app) restart() {
	if a.restarting {
		return
	}
	a.restarting = true
	a.notice = "正在重启 daemon"
	go func() {
		err := dash.RestartDaemon(context.Background())
		mygo.RunOnMain(func() {
			a.restarting = false
			if err != nil {
				a.notice = "重启失败:" + err.Error()
			} else {
				a.notice = "已重启 daemon(" + time.Now().Format("15:04:05") + ")"
			}
			if a.win != nil {
				a.win.Invalidate()
			}
		})
		time.Sleep(3 * time.Second) // let the new process beat before rereading
		a.refreshNow()
	}()
}
