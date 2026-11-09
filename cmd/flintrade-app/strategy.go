package main

import (
	"os"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/editor"

	"flintrade/internal/dash"
)

type stratFile struct {
	file dash.File
	disk string // content on disk when last loaded or saved
	ed   *editor.State
}

type strategyState struct {
	files      []*stratFile
	names      []string
	sel        string
	err        string // why the last save attempt was refused
	msg        string
	confirm    bool // the diff dialog is open
	diff       *editor.DiffState
	askRestart bool
}

func (a *app) loadStrategy() {
	st := &a.strat
	st.files, st.names = nil, nil
	for _, f := range dash.ConfigFiles(a.root) {
		b, err := os.ReadFile(f.Path)
		if err != nil {
			continue
		}
		buf := editor.NewBuffer(string(b))
		buf.SetLanguage(editor.LanguageFor(f.Path))
		st.files = append(st.files, &stratFile{file: f, disk: string(b),
			ed: &editor.State{Buffer: buf, SoftWrap: true, ReadOnly: f.Gated}})
		st.names = append(st.names, f.Name)
	}
	if st.sel == "" && len(st.names) > 0 {
		st.sel = st.names[0]
	}
}

func (a *app) current() *stratFile {
	for _, f := range a.strat.files {
		if f.file.Name == a.strat.sel {
			return f
		}
	}
	return nil
}

// reload rereads the selected file from disk, dropping unsaved edits.
func (sf *stratFile) reload() error {
	b, err := os.ReadFile(sf.file.Path)
	if err != nil {
		return err
	}
	sf.disk = string(b)
	sf.ed.Buffer.Reset(sf.disk)
	return nil
}

func (a *app) strategyPage(c *ui.Context) {
	t := c.Theme()
	st := &a.strat
	if st.files == nil {
		a.loadStrategy()
	}
	cur := a.current()
	ui.Column(c).Grow(1).Padding(pageInset...).Gap(12).Children(func() {
		ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
			sectionTitle(c, "策略")
			if ui.Select(c, &st.sel, st.names).Width(280).Changed() {
				st.err, st.msg = "", ""
			}
			ui.Spacer(c)
			if cur == nil {
				return
			}
			modified := cur.ed.Buffer.String() != cur.disk
			if ui.Button(c, "放弃修改").Disabled(!modified).Clicked() {
				st.err, st.msg = "", ""
				_ = cur.reload()
			}
			if ui.PrimaryButton(c, "检查并保存").Disabled(!modified || cur.file.Gated).Clicked() {
				st.msg = ""
				if err := dash.Validate(cur.file, cur.ed.Buffer.String()); err != nil {
					st.err = err.Error()
				} else {
					st.err = ""
					st.diff = &editor.DiffState{Old: editor.NewBuffer(cur.disk), New: editor.NewBuffer(cur.ed.Buffer.String()),
						Layout: editor.DiffUnified}
					st.confirm = true
				}
			}
		})
		if cur == nil {
			ui.Text(c, "没有可编辑的文件").TextColor(t.TextMuted)
			return
		}
		switch {
		case cur.file.Gated:
			ui.Text(c, "风控数值改动必须先过回测(CLAUDE.md)。Go 版回测落地前这里只读。").FontSize(12).TextColor(t.TextMuted)
		case cur.file.TOML:
			ui.Text(c, "保存前按 daemon 的严格规则解析:未知字段、类型错误都会被拒。daemon 重启后生效。").FontSize(12).TextColor(t.TextMuted)
		default:
			ui.Text(c, "生产者的 system prompt:改的是交易判断。改完建议跑回测再上线。daemon 重启后生效。").FontSize(12).TextColor(t.TextMuted)
		}
		if st.err != "" {
			ui.Text(c, st.err).TextColor(t.Accent).Selectable()
		}
		if st.msg != "" {
			ui.Text(c, st.msg).TextColor(t.TextMuted)
		}
		editor.View(c, cur.ed).Grow(1).FillWidth()
	})

	if st.confirm && cur != nil {
		ui.Modal(c, &st.confirm, func() {
			ui.Column(c).Width(880).Height(560).Padding(20).Gap(12).Children(func() {
				ui.Text(c, "保存 "+cur.file.Name).FontSize(16).Bold()
				editor.DiffView(c, st.diff).Grow(1).FillWidth()
				ui.Row(c).Gap(8).Justify(ui.End).Children(func() {
					if ui.Button(c, "取消").Clicked() {
						st.confirm = false
					}
					save := func(restart bool) {
						st.confirm = false
						content := cur.ed.Buffer.String()
						if err := dash.Save(cur.file, content); err != nil {
							st.err = err.Error()
							return
						}
						cur.disk = content
						cur.ed.Buffer.MarkSaved()
						st.msg = "已保存 " + cur.file.Path
						if restart {
							a.restart()
						} else {
							st.msg += ";daemon 重启后生效"
						}
					}
					if ui.Button(c, "只保存").Clicked() {
						save(false)
					}
					if ui.PrimaryButton(c, "保存并重启 daemon").Clicked() {
						save(true)
					}
				})
			})
		})
	}
}
