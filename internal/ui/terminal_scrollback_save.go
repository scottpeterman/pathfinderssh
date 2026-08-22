package ui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/storage"
)

// ScrollbackText returns the full virtual buffer (history + live screen) as
// plain text, one line per row. Empty when the screen is not ready.
func (t *NativeTerminalWidget) ScrollbackText() string {
	if t == nil || t.screen == nil {
		return ""
	}
	total := t.screen.GetTotalContentLines()
	if total <= 0 {
		return ""
	}
	lines := t.screen.GetLinesInRange(0, total)
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n") + "\n"
}

// SaveScrollback writes the current buffer to path. Creates parent dirs as needed.
func (t *NativeTerminalWidget) SaveScrollback(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("no path")
	}
	text := t.ScrollbackText()
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("nothing in the scrollback to save")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(text), 0o644)
}

// DefaultScrollbackSavePath suggests a filename under the transcript directory.
func DefaultScrollbackSavePath(sessionName string) string {
	cfg := CurrentSettings()
	dir := cfg.LogDirectory
	if dir == "" {
		dir = GetLogsDir()
	}
	safe := sanitizeLogName(sessionName)
	if safe == "" {
		safe = "session"
	}
	return filepath.Join(dir, fmt.Sprintf("%s_scrollback_%s.txt", safe, time.Now().Format("20060102_150405")))
}

// PromptSaveScrollback opens a file save dialog for the current buffer.
func (t *NativeTerminalWidget) PromptSaveScrollback() {
	t.promptSaveScrollback()
}

// promptSaveScrollback opens a file save dialog for the current buffer.
func (t *NativeTerminalWidget) promptSaveScrollback() {
	win := t.HostWindow()
	if win == nil {
		return
	}
	name := "session"
	suggested := DefaultScrollbackSavePath(name)
	d := dialog.NewFileSave(func(wc fyne.URIWriteCloser, err error) {
		if err != nil {
			dialog.ShowError(err, win)
			return
		}
		if wc == nil {
			return
		}
		defer wc.Close()
		text := t.ScrollbackText()
		if strings.TrimSpace(text) == "" {
			dialog.ShowInformation("Save Scrollback", "Nothing in the scrollback to save.", win)
			return
		}
		if _, err := wc.Write([]byte(text)); err != nil {
			dialog.ShowError(err, win)
			return
		}
		dialog.ShowInformation("Save Scrollback", "Saved to "+wc.URI().Path(), win)
	}, win)
	d.SetFileName(filepath.Base(suggested))
	d.SetFilter(storage.NewExtensionFileFilter([]string{".txt", ".log"}))
	if dir := filepath.Dir(suggested); dir != "" {
		if l, err := storage.ListerForURI(storage.NewFileURI(dir)); err == nil {
			d.SetLocation(l)
		}
	}
	d.Resize(fyne.NewSize(820, 600))
	d.Show()
}
