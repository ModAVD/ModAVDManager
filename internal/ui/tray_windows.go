package ui

import (
	"modavd/internal/bosskey"
	"modavd/internal/win"
)

// realNewTray is the manager's win.Tray.
func realNewTray(tip string, ev trayEvents) (trayHost, error) {
	t, err := win.StartTray(win.TrayOptions{Tip: tip, Events: win.TrayEvents{HotKey: ev.hotKey, Click: ev.click,
		DoubleClick: ev.doubleClick, Open: ev.open, Exit: ev.exit}})
	if err != nil {
		return nil, err
	}
	return winTray{t}, nil
}

type winTray struct{ *win.Tray }

func (t winTray) RegisterHotKey(id int, k bosskey.Key) error {
	return t.Tray.RegisterHotKey(id, k.Modifiers(), k.VK)
}

func (t winTray) TryHotKey(k bosskey.Key) error { return t.Tray.TryHotKey(k.Modifiers(), k.VK) }
