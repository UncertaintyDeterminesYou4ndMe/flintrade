// Command flintrade-app is Flintrade.app: a menu bar status item plus a window to
// read the ledger, edit strategy files and replace Longbridge credentials.
//
// It reads flintrade.db read-only and never orders. Its only writes are config
// and prompt files, the credentials file, and `launchctl kickstart` to make
// the daemon pick them up. Quitting the app does not stop trading.
package main

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/i18n"
	"github.com/egoist/mygo/ui"

	"flintrade/cmd/flintrade-app/locales"
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
	langTag        string // language chosen in the tray menu; "" follows the system
	menuBar        string // what the menu bar title shows: "" status only, "pnl", "pct"
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
		a.loadLanguage()
		a.menuBar = loadPref("flintrade-menubar")
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
		// The tray title and menu say words of the language; the window's
		// title is the product name, and its content draws anew by itself.
		mygo.App.OnLocaleChanged(func(string) {
			a.applyTray()
			if a.win != nil {
				a.win.Invalidate()
			}
		})
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
		opts := make([]mygo.NotificationOptions, len(alerts))
		for i, al := range alerts {
			title, body := alertText(al)
			opts[i] = mygo.NotificationOptions{ID: "flintrade-" + al.Key, Title: title, Body: body}
		}
		go func() {
			for _, o := range opts {
				if err := mygo.NewNotification(o).Show(); err != nil {
					log.Printf("notify %s: %v", o.ID, err)
				}
			}
		}()
	}
}

// alertText is a notification's title and body in the app's language.
func alertText(al dash.Alert) (title, body string) {
	switch al.Key {
	case "down":
		return locales.AlertDownTitle(), locales.AlertDownBody(strings.Join(al.Loops, locales.CommonListSeparator()))
	case "up":
		return locales.AlertUpTitle(), locales.AlertUpBody()
	case "halt":
		if al.Reason == "" {
			return locales.AlertHaltTitle(), locales.AlertHaltNoReason()
		}
		return locales.AlertHaltTitle(), al.Reason
	case "token":
		return locales.AlertTokenTitle(), al.Message
	}
	return "Flintrade", al.Key
}

// errText says an error from internal/dash in the app's language; any other
// error as it is.
func errText(err error) string {
	var rejected *dash.CredsRejectedError
	switch {
	case errors.Is(err, dash.ErrGated):
		return locales.ErrGated()
	case errors.Is(err, dash.ErrEmpty):
		return locales.ErrEmpty()
	case errors.Is(err, dash.ErrNoChanges):
		return locales.ErrNoChanges()
	case errors.As(err, &rejected):
		return locales.ErrCredsRejected(rejected.Err.Error(), rejected.Output)
	}
	return err.Error()
}

// trayTitle is what the menu bar shows: what is wrong, else the name, with
// today's P&L only when the menu says to (a bare number there means nothing
// to anyone who has not been told what it is).
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
		return locales.TrayStopped()
	case s.Halted():
		return locales.TrayHalted()
	}
	if d := s.Day(); d != nil {
		switch a.menuBar {
		case "pnl":
			return "Flintrade " + signed(d.PnL)
		case "pct":
			if d.Base != 0 {
				return "Flintrade " + pct(d.PnL/d.Base*100)
			}
		}
	}
	return "Flintrade"
}

// dayLine is today's P&L for the tray menu, in dollars and percent, saying
// when it counts realized P&L only; "" when there is no figure.
func (a *app) dayLine() string {
	if a.snap == nil {
		return ""
	}
	d := a.snap.Day()
	if d == nil || d.Base == 0 {
		return ""
	}
	if d.WithFloat {
		return locales.TrayDayPnL(signed(d.PnL), pct(d.PnL/d.Base*100))
	}
	return locales.TrayDayRealized(signed(d.PnL), pct(d.PnL/d.Base*100))
}

func (a *app) statusLine() string {
	s := a.snap
	if a.rootErr != nil {
		return locales.StatusRepoMissing()
	}
	if s == nil {
		return locales.StatusLoading()
	}
	mode := "DRY RUN"
	if s.Live {
		mode = "LIVE paper"
	}
	daemon := locales.StatusDaemonRunning()
	if s.DaemonDown() {
		daemon = locales.StatusDaemonStopped()
	} else if s.Halted() {
		daemon = locales.StatusEntriesPaused()
	}
	eq := "—"
	if n := s.NetEquity(); n != nil {
		eq = money(*n)
	}
	return locales.StatusLine(mode, daemon, eq)
}

// dayRollNote says when "today" restarts: the risk monitor rolls the day at
// midnight New York time, which is 13:00 (12:00 in US summer) on a Beijing
// clock; the note gives that moment on the Mac's own clock.
func dayRollNote(now time.Time) string {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		return locales.StatusDayRollFallback()
	}
	n := now.In(ny)
	midnight := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, ny).In(now.Location())
	return locales.StatusDayRoll(midnight.Format("15:04"))
}

func (a *app) trayMenu() *mygo.Menu {
	items := []*mygo.MenuItem{{ID: "status", Label: a.statusLine(), Disabled: true}}
	if l := a.dayLine(); l != "" {
		items = append(items, &mygo.MenuItem{ID: "pnl", Label: l, Disabled: true})
	}
	return mygo.NewMenu(append(items,
		&mygo.MenuItem{ID: "day", Label: dayRollNote(time.Now()), Disabled: true},
		mygo.Separator(),
		&mygo.MenuItem{ID: "open", Label: locales.TrayOpen(), Click: func(*mygo.MenuItem, *mygo.Window) { a.open() }},
		&mygo.MenuItem{ID: "menubar", Label: locales.TrayMenuBar(), Submenu: a.menuBarItems()},
		&mygo.MenuItem{ID: "language", Label: locales.TrayLanguage(), Submenu: a.languageItems()},
		&mygo.MenuItem{ID: "restart", Label: locales.CommonRestartDaemon(), Click: func(*mygo.MenuItem, *mygo.Window) { a.restart() }},
		mygo.Separator(),
		&mygo.MenuItem{ID: "quit", Label: locales.TrayQuit(), Accelerator: "CmdOrCtrl+Q", Click: func(*mygo.MenuItem, *mygo.Window) { mygo.App.Quit() }},
	))
}

// menuBarItems is the choice of what the menu bar title shows.
func (a *app) menuBarItems() []*mygo.MenuItem {
	var items []*mygo.MenuItem
	for _, c := range []struct{ v, label string }{
		{"", locales.TrayShowStatus()}, {"pnl", locales.TrayShowPnL()}, {"pct", locales.TrayShowPct()},
	} {
		items = append(items, &mygo.MenuItem{ID: "menubar-" + c.v, Label: c.label, Type: mygo.MenuItemRadio,
			Checked: a.menuBar == c.v,
			Click: func(*mygo.MenuItem, *mygo.Window) {
				a.menuBar = c.v
				savePref("flintrade-menubar", c.v)
				a.applyTray()
			}})
	}
	return items
}

// languageItems is the language choice: follow the system, or one of the
// app's languages named in itself. SetLocale reports back through
// OnLocaleChanged, which rebuilds the menu.
func (a *app) languageItems() []*mygo.MenuItem {
	cur := mygo.App.Locale()
	items := []*mygo.MenuItem{{ID: "lang-system", Label: locales.TrayLanguageSystem(), Type: mygo.MenuItemRadio,
		Checked: a.langTag == "",
		Click:   func(*mygo.MenuItem, *mygo.Window) { a.setLanguage("") }}}
	for _, tag := range mygo.App.Languages() {
		items = append(items, &mygo.MenuItem{ID: "lang-" + tag, Label: i18n.Autonym(tag), Type: mygo.MenuItemRadio,
			Checked: a.langTag != "" && tag == cur,
			Click:   func(*mygo.MenuItem, *mygo.Window) { a.setLanguage(tag) }})
	}
	return items
}

// prefFile is a tray choice kept in the app's data directory: the language
// (MyGo cannot say whether the app follows the system or was told one) and
// what the menu bar shows.
func prefFile(name string) string {
	dir, err := mygo.App.Path(mygo.PathUserData)
	if err != nil {
		return ""
	}
	return filepath.Join(dir, name)
}

func loadPref(name string) string {
	if p := prefFile(name); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return ""
}

func savePref(name, v string) {
	if p := prefFile(name); p != "" {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err == nil {
			_ = os.WriteFile(p, []byte(v), 0o644)
		}
	}
}

func (a *app) loadLanguage() { a.langTag = loadPref("flintrade-language") }

// setLanguage shows tag, or follows the system language for "".
func (a *app) setLanguage(tag string) {
	a.langTag = tag
	savePref("flintrade-language", tag)
	mygo.App.SetLocale(tag)
	a.applyTray() // when the language does not change, nothing else rebuilds the radio marks
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
	a.notice = locales.StatusRestarting()
	go func() {
		err := dash.RestartDaemon(context.Background())
		mygo.RunOnMain(func() {
			a.restarting = false
			if err != nil {
				a.notice = locales.StatusRestartFailed(err.Error())
			} else {
				a.notice = locales.StatusRestarted(time.Now().Format("15:04:05"))
			}
			if a.win != nil {
				a.win.Invalidate()
			}
		})
		time.Sleep(3 * time.Second) // let the new process beat before rereading
		a.refreshNow()
	}()
}
