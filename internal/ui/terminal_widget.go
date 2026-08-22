// internal/ui/terminal_widget.go
package ui

import (
	"context"
	"errors"
	"log"
	"math"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/widget"

	"github.com/scottpeterman/pathfinderssh/internal/gopyte"
)

var (
	_ fyne.Focusable    = (*NativeTerminalWidget)(nil)
	_ fyne.Tappable     = (*NativeTerminalWidget)(nil)
	_ fyne.Draggable    = (*NativeTerminalWidget)(nil)
	_ desktop.Hoverable = (*NativeTerminalWidget)(nil)
	_ fyne.Shortcutable = (*NativeTerminalWidget)(nil)
	// _ desktop.Keyable   = (*NativeTerminalWidget)(nil)
)

// NativeTerminalWidget - Enhanced with cross-platform PTY and history support
type NativeTerminalWidget struct {
	widget.BaseWidget
	fyne.ShortcutHandler // ADD: Embed the shortcut handler for control keys
	lineSelection        struct {
		startLine int
		endLine   int
		active    bool
	}
	// Core components - Using WideCharScreen with enhanced history
	screen    *gopyte.WideCharScreen
	stream    *gopyte.Stream
	textGrid  *widget.TextGrid
	baseBG    *canvas.Rectangle // opaque terminal pane background (decoupled from chrome)
	bgLayer   *bgLayer          // cell/selection backgrounds, drawn behind the transparent grid
	scroll    *HybridScrollContainer
	scrollbar *VirtualScrollbar
	selection *SelectionManager

	// State management
	title string

	// hostCanvas overrides the canvas this terminal believes it is on.
	//
	// It exists because fyne's object->canvas cache CANNOT be updated. Its
	// writer is cache.SetCanvasForObject, which uses LoadOrStore: once an
	// object has been rendered on one canvas, a later render on a DIFFERENT
	// canvas keeps the original entry. So for a widget that has been moved
	// between windows, CanvasForObject permanently returns the window it
	// was first shown in -- focus, popup menus and dialogs all go there
	// instead, silently, while the widget renders correctly where it now
	// lives. Set this on every move; nil restores the cache lookup.
	hostCanvasMu sync.RWMutex
	hostCanvas   fyne.Canvas

	// pasteLineDelayMs paces multi-line paste, read at construction like
	// fontSize and scrollback. It is a field rather than a live read of the
	// global settings because a tabbed shell has several terminals alive at
	// once: reading the global at paste time would give a session whatever
	// value the most recently opened tab installed.
	pasteLineDelayMs int

	// pasteConsoleBaud paces paste within a line, read at construction for
	// the same reason: it is a property of the device this tab is connected
	// to, and the global holds whichever session mounted last.
	pasteConsoleBaud int

	// pasteWarnLines is the line count at or above which a paste asks first.
	// Read at construction with the other two, and for the same reason.
	// Zero means never ask.
	pasteWarnLines int

	// pasteRemember, when set, persists a pacing override chosen in the
	// confirmation dialog. It is a hook rather than a write from here
	// because this package owns no inventory: only the host knows which
	// node this tab came from and where that node is stored. Nil means
	// nothing can remember it, and the dialog then hides the checkbox
	// rather than offering a promise it cannot keep.
	pasteRemember func(lineDelayMs, consoleBaud int)

	// targetLabel names the device for anything that has to say WHICH
	// session it is about to act on. Separate from title, which is the OSC-0
	// window title: no network device sets one, so title answers "Terminal"
	// forever and cannot be used for this.
	targetLabel string

	// Font and sizing
	fontSize   float32
	charWidth  float32
	charHeight float32
	cols       int
	rows       int

	// Enhanced virtual scrolling state
	virtualScroll VirtualScrollState

	// Thread safety and performance
	mutex sync.RWMutex

	// updatePending is set by every goroutine that dirties the screen (both feed
	// paths, key/paste/selection/find handlers, resize, theme changes) and read
	// by the render ticker. It was a plain bool, which -race flagged from six
	// call sites; atomic makes those writes legal and lets the ticker
	// test-and-clear without a mutex.
	updatePending atomic.Bool

	// remoteResize coalesces SSH WindowChange onto one background worker.
	remoteCols     atomic.Int32
	remoteRows     atomic.Int32
	remoteResizeOn atomic.Bool

	// Context for cancellation
	ctx    context.Context
	cancel context.CancelFunc

	// Size change detection
	lastWidth   float32
	lastHeight  float32
	resizeTimer *time.Timer
	resizeMutex sync.Mutex

	// Selection support
	selectionStart fyne.Position
	selectionEnd   fyne.Position
	isSelecting    bool

	// Theme

	// Default colors
	fgColor string
	bgColor string

	// Performance optimization
	cachedLines []string

	// Event handling state
	hasFocus       bool
	debugEvents    bool
	lastScrollTime time.Time

	// Cross-platform compatibility flags
	isWindows bool
	isUnix    bool

	// writeOverride is the sink for everything the user types. It is the only
	// one: pfssh has no local-shell path, so a widget with no override set is
	// simply not connected to anything and input is dropped.
	writeOverride func([]byte)

	// Live session-logging hooks, injected by the owning Session (nil when the
	// widget is standalone, which then shows no logging item in the menu).
	isLoggingFn     func() bool
	toggleLoggingFn func() (bool, string)

	// Tab-management hooks, injected by the SessionManager when the terminal is
	// hosted in a tab (nil otherwise, so the items are hidden). These drive the
	// "Close Other Tabs" / "Close All Tabs" entries in the right-click menu.
	closeAllTabsFn   func()
	closeOtherTabsFn func()
	tabCountFn       func() int

	// Resize callback - allows SSH sessions to receive resize events
	onResizeCallback func(cols, rows int)

	// Paste pacing (see terminal_paste.go)
	pasteMutex  sync.Mutex
	pasteCancel context.CancelFunc

	// bracketedPaste tracks whether the remote enabled DEC mode 2004 (set in
	// handleEscapeSequences). When true, pasteText wraps content in paste markers.
	bracketedPaste atomic.Bool

	// modeTail carries the last few bytes of output across reads so a DEC
	// 2004 sequence split by a read boundary is still recognised. Guarded
	// by its own mutex because the two output paths -- the local feed and
	// the SSH read loop -- are different goroutines.
	modeTailMu sync.Mutex
	modeTail   string

	// redrawing is the in-flight guard for the 60fps update loop
	// (enhancedUpdateProcessor). It is set true while a performRedrawDirect is
	// queued/running on the main thread and cleared when it completes, so the
	// ticker can never stack a second full repaint behind an unfinished one.
	// Without it, a frame that paints slower than the tick interval (e.g. btop's
	// truecolor full-screen) backs up the main-thread queue and starves input.
	redrawing atomic.Bool

	// redrawStarted is the UnixNano at which the in-flight paint was
	// dispatched, and it is the escape hatch that makes the guard safe to
	// hold. A guard released only by its own closure is permanent if that
	// closure never runs -- a queue drained during teardown, a render that
	// panics somewhere the recover does not cover -- and the symptom is a
	// terminal that stops painting for good. That is what the guard was
	// disabled to investigate. Now a paint that has been in flight longer
	// than redrawStaleAfter is treated as lost and a fresh one is allowed,
	// so the worst case degrades to the unguarded behaviour for one tick
	// instead of wedging the widget.
	redrawStarted atomic.Int64

	// Per-terminal terminal-theme scope (see cli/terminal_theme_scope.go). The
	// zero value inherits the global Settings -> Terminal Theme, so every
	// existing NativeTerminalWidget{...} literal keeps working unchanged.
	termTheme termThemeScope

	// Find-in-scrollback bar (see cli/terminal_find.go). Built lazily in
	// CreateRenderer; nil until the widget is first rendered.
	find *findController

	// focusHost is the object that is ACTUALLY in the canvas object tree, which
	// for an SSH/telnet/serial tab is the *Session wrapper, not this
	// inner widget. Focus must be requested for that object - see focusObject.
	// nil (a bare PTY terminal) means "this widget is the tree object".
	focusHost focusableObject
}

// focusableObject is a canvas object that can also take keyboard focus - the
// shape shared by *NativeTerminalWidget and its *Session wrapper.
type focusableObject interface {
	fyne.CanvasObject
	fyne.Focusable
}

// SetFocusHost records the wrapper widget that hosts this terminal in the object
// tree. Call it from the wrapper's constructor.
//
// Why this exists: Fyne's FocusManager.Focus resolves its target by OBJECT
// IDENTITY, walking the tree for `object == obj`. *Session embeds
// *NativeTerminalWidget, so the tree holds the outer wrapper while this inner
// widget is what most of the code has a handle to. Passing the inner widget to
// Canvas.Focus never matches, Focus returns false, and nothing is focused -
// silently. The same identity problem applies to CanvasForObject, whose cache is
// keyed by the rendered (outer) object.
func (t *NativeTerminalWidget) SetFocusHost(host focusableObject) {
	t.focusHost = host
}

// SetTabHooks injects the tab-management actions the right-click menu offers.
//
// The terminal must not know what a tab is, so these arrive as three plain
// functions from whoever is hosting it: close every tab, close the others, and
// how many there are (which is only used to grey out "close others" when this
// is the only one). A standalone terminal leaves them nil and the menu shows
// no tab entries at all.
//
// Passing a nil closeAll leaves the whole group hidden, which is what a host
// that has no tab strip should do.
func (t *NativeTerminalWidget) SetTabHooks(closeAll, closeOthers func(), count func() int) {
	t.closeAllTabsFn = closeAll
	t.closeOtherTabsFn = closeOthers
	t.tabCountFn = count
}

// focusObject is the object to hand to Canvas.Focus / CanvasForObject.
func (t *NativeTerminalWidget) focusObject() focusableObject {
	if t.focusHost != nil {
		return t.focusHost
	}
	return t
}

// GrabFocus gives keyboard focus to this terminal and reports whether it could.
// Every focus request in the terminal should go through here rather than
// calling Canvas.Focus directly, so the identity rule above is applied once.
func (t *NativeTerminalWidget) GrabFocus() bool {
	obj := t.focusObject()
	// Through FocusCanvas, not the driver cache directly: a moved terminal
	// would otherwise hand focus to the window it used to be in on every
	// click, which looks exactly like a terminal that has stopped
	// accepting input.
	c := t.FocusCanvas()
	if c == nil {
		return false
	}
	c.Focus(obj)
	return true
}

// SetHostCanvas records the canvas this terminal is currently displayed on,
// overriding the driver's cache. Pass nil to go back to the cache.
//
// A host that can MOVE a terminal between windows must call this on every
// move. See the hostCanvas field for why the cache cannot be trusted after
// one. A host that never moves anything never needs to call it.
func (t *NativeTerminalWidget) SetHostCanvas(c fyne.Canvas) {
	t.hostCanvasMu.Lock()
	t.hostCanvas = c
	t.hostCanvasMu.Unlock()
}

// FocusCanvas is the canvas this terminal lives on, resolved through the same
// host object as GrabFocus. Returns nil before the widget has been rendered.
func (t *NativeTerminalWidget) FocusCanvas() fyne.Canvas {
	t.hostCanvasMu.RLock()
	c := t.hostCanvas
	t.hostCanvasMu.RUnlock()
	if c != nil {
		return c
	}
	return fyne.CurrentApp().Driver().CanvasForObject(t.focusObject())
}

// HostWindow is the window this terminal is currently displayed in.
//
// It is resolved rather than remembered because a terminal can be MOVED: the
// shell can pull a live session out of the tab strip into a window of its own,
// and back. Anything that cached a window at construction would then put a
// popup or a dialog on the window the session used to be in. The old code here
// took AllWindows()[0], which is that bug with the main window hard-coded.
//
// Returns nil before the widget has been rendered.
func (t *NativeTerminalWidget) HostWindow() fyne.Window {
	c := t.FocusCanvas()
	if c == nil {
		return nil
	}
	for _, w := range fyne.CurrentApp().Driver().AllWindows() {
		if w.Canvas() == c {
			return w
		}
	}
	return nil
}

// HasTerminalFocus reports whether the canvas focus currently sits on this
// terminal (i.e. on its host object).
func (t *NativeTerminalWidget) HasTerminalFocus() bool {
	c := t.FocusCanvas()
	if c == nil {
		return false
	}
	return c.Focused() == fyne.Focusable(t.focusObject())
}

// NewNativeTerminalWidget creates a new cross-platform terminal with history
// support. It takes no light/dark argument: a terminal's contrast comes from
// its own palette's `type` field, resolved per widget (see termIsDark), never
// from the application chrome. Use SetTerminalTheme to give one session a
// palette of its own.
func NewNativeTerminalWidget() *NativeTerminalWidget {
	ctx, cancel := context.WithCancel(context.Background())

	t := &NativeTerminalWidget{
		ctx:    ctx,
		cancel: cancel,
		// Terminal font size comes from Settings -> Terminal -> Font Size (read
		// at construction, so a change applies to newly connected sessions). It
		// is the initial estimate for charWidth/charHeight; once the grid
		// renders, gridCellSize() measures the real cell and everything tracks
		// that. The grid is made to actually RENDER at this size by wrapping the
		// terminal in a container.NewThemeOverride (see SessionManager tab setup).
		fontSize:         float32(ClampTerminalFontSize(CurrentSettings().FontSize)),
		pasteLineDelayMs: CurrentSettings().PasteLineDelayMs,
		pasteConsoleBaud: CurrentSettings().PasteConsoleBaud,
		pasteWarnLines:   CurrentSettings().PasteWarnLines,
		cols:             80,
		rows:             24,
		title:            "Terminal",
		fgColor:          "white",
		bgColor:          "black",
		cachedLines:      make([]string, 0, 150),

		// Platform detection
		isWindows: runtime.GOOS == "windows",
		isUnix:    runtime.GOOS != "windows",

		// Event handling
		hasFocus:       false,
		debugEvents:    true,
		lastScrollTime: time.Now(),

		// Initialize virtual scroll state
		virtualScroll: VirtualScrollState{
			visibleLines: 24,
		},
	}

	t.selection = NewSelectionManager(t)

	t.calculateCharDimensions()

	// Scrollback depth. Default is platform-sensible; a positive value from
	// Settings -> Terminal -> Scrollback Lines overrides it. Read at
	// construction, so a changed setting takes effect on the next connection.
	historyLines := 1000
	if runtime.GOOS == "windows" {
		// ConPTY can handle more history efficiently
		historyLines = 2000
	}
	if n := CurrentSettings().ScrollbackLines; n > 0 {
		historyLines = n
	}

	log.Printf("Creating WideCharScreen with enhanced history support (%d lines)", historyLines)
	t.screen = gopyte.NewWideCharScreen(t.cols, t.rows, historyLines)
	t.stream = gopyte.NewStream(t.screen, false)

	// Create TextGrid
	t.textGrid = widget.NewTextGrid()
	t.textGrid.ShowLineNumbers = false
	t.textGrid.ShowWhitespace = false

	// Initialize TextGrid size
	t.initializeTextGridSize()

	// Start background processing
	go t.updateProcessor()

	t.ExtendBaseWidget(t)
	log.Printf("NewNativeTerminalWidget: Created %s terminal widget", runtime.GOOS)
	return t
}

// INTERFACE IMPLEMENTATIONS - Enhanced with unified system
func (t *NativeTerminalWidget) Focusable() bool {
	return true
}

func (t *NativeTerminalWidget) FocusLost() {
	dprintf("FocusLost: Unified terminal widget lost focus (%s)\n", runtime.GOOS)
	t.hasFocus = false
}

// errNoTransport is returned by sendInput when nothing is attached to receive
// the bytes. Paste uses it to abort a multi-line burst rather than spraying the
// remainder into a session that has gone away.
var errNoTransport = errors.New("terminal: no transport attached")

// canSend reports whether there is somewhere for input to go.
func (t *NativeTerminalWidget) canSend() bool { return t.writeOverride != nil }

// sendInput hands keyboard input to whatever transport owns this widget. It
// replaces TetherSSH's WriteToPTY, which checked writeOverride first and fell
// back to a local pty. pfssh has no local-shell path, so only the override
// remains -- but the error return is kept, because the paste loop needs to know
// when to stop.
func (t *NativeTerminalWidget) sendInput(data []byte) error {
	if t.writeOverride == nil {
		dprintf("sendInput: no transport attached, dropping %d bytes\n", len(data))
		return errNoTransport
	}
	t.writeOverride(data)
	return nil
}

func (t *NativeTerminalWidget) TypedShortcut(shortcut fyne.Shortcut) {
	dprintf("TypedShortcut received: %T\n", shortcut)

	if !t.canSend() {
		dprintf("TypedShortcut: no transport attached, ignoring\n")
		return
	}

	// Handle desktop custom shortcuts (Ctrl+key and Alt+key combinations)
	if customShortcut, ok := shortcut.(*desktop.CustomShortcut); ok {
		dprintf("Custom shortcut detected: Key=%s, Modifier=%d\n",
			customShortcut.KeyName, customShortcut.Modifier)
		// Find in scrollback. Checked before the control-byte path below, which
		// would otherwise swallow F as Ctrl-F (0x06). Ctrl+SHIFT+F rather than
		// Ctrl+F so plain Ctrl-F still reaches the remote shell (readline
		// forward-char, and the tmux prefix people rebind to it).
		//
		// With the main menu attached, Edit -> Find… holds this accelerator and
		// Fyne dispatches it there first, so this branch is the fallback path.
		// Kept deliberately: it keeps the terminal self-contained if it is ever
		// hosted in a window without the menu.
		if customShortcut.Modifier&fyne.KeyModifierControl != 0 &&
			customShortcut.Modifier&fyne.KeyModifierShift != 0 &&
			customShortcut.KeyName == fyne.KeyF {
			if t.find != nil {
				t.find.Open()
			}
			return
		}
		// In TypedShortcut method:
		if customShortcut.Modifier&fyne.KeyModifierControl != 0 {
			if customShortcut.KeyName == fyne.KeyC {
				if t.selection != nil && t.selection.HasSelection() {
					// Copy selection and clear
					t.selection.CopyToClipboard()
					t.selection.Clear()
					dprintf("Copied selection and cleared\n")
					return
				} else {
					// No selection - send interrupt
					t.sendInput([]byte{0x03})
					return
				}
			}
		}
		// Handle Alt modifier combinations FIRST (before Control)
		if customShortcut.Modifier&fyne.KeyModifierAlt != 0 {
			// Alt+key sends ESC followed by the key
			var sequence []byte

			// Check if it's Alt+Control combo (handle differently)
			if customShortcut.Modifier&fyne.KeyModifierControl != 0 {
				// Alt+Ctrl combinations - less common but some apps use them
				dprintf("Alt+Ctrl combo detected\n")
				// You can implement specific handling here if needed
				return
			}

			// Pure Alt+key combinations
			if keyChar := t.keyNameToChar(customShortcut.KeyName); keyChar != 0 {
				sequence = []byte{0x1B, keyChar} // ESC + character
				dprintf("Sending Alt+%c sequence: ESC+%c (0x1B 0x%02X)\n",
					keyChar, keyChar, keyChar)
			} else {
				// Handle special keys with Alt
				switch customShortcut.KeyName {
				case fyne.KeyLeft:
					sequence = []byte{0x1B, 0x5B, 0x44} // ESC[D
				case fyne.KeyRight:
					sequence = []byte{0x1B, 0x5B, 0x43} // ESC[C
				case fyne.KeyUp:
					sequence = []byte{0x1B, 0x5B, 0x41} // ESC[A
				case fyne.KeyDown:
					sequence = []byte{0x1B, 0x5B, 0x42} // ESC[B
				case fyne.KeyBackspace:
					sequence = []byte{0x1B, 0x7F} // ESC + DEL
				case fyne.KeyReturn:
					sequence = []byte{0x1B, 0x0D} // ESC + CR
				}
			}

			if len(sequence) > 0 {
				t.sendInput(sequence)
				t.updatePending.Store(true)
				if t.screen != nil {
					t.screen.InvalidateCache()
				}
				t.triggerImmediateRedraw()
				return
			}
		}

		// Handle Control modifier combinations (existing code)
		if customShortcut.Modifier&fyne.KeyModifierControl != 0 {
			controlByte := t.keyNameToControlByte(customShortcut.KeyName)
			if controlByte != 0 {
				dprintf("Sending control sequence: Ctrl+%c (0x%02X)\n",
					controlByte+64, controlByte)

				t.sendInput([]byte{controlByte})
				t.updatePending.Store(true)

				if t.screen != nil {
					t.screen.InvalidateCache()
				}

				t.triggerImmediateRedraw()
				return
			}
		}
	}

	// Handle standard shortcuts (Copy, Paste, Cut, etc.)
	switch shortcut := shortcut.(type) {
	case *fyne.ShortcutCopy:
		// Ctrl+C interrupts the remote process; also abort any in-flight paste.
		t.cancelActivePaste()
		t.sendInput([]byte{0x03}) // Ctrl+C

	case *fyne.ShortcutPaste:
		if shortcut.Clipboard != nil {
			t.pasteText(shortcut.Clipboard.Content())
		}

	case *fyne.ShortcutCut:
		dprintf("Cut shortcut (Ctrl+X) detected\n")
		t.sendInput([]byte{0x18}) // Ctrl+X

	// On Linux/Windows the platform "shortcut modifier" is Ctrl, so the Fyne
	// desktop driver rewrites Ctrl+A/Ctrl+Z/Ctrl+Y into these built-in editing
	// shortcuts BEFORE the desktop.CustomShortcut path (and keyNameToControlByte)
	// ever runs. In a terminal those keys belong to the remote shell, not the GUI:
	// Ctrl+A is readline beginning-of-line (and the tmux/screen prefix), Ctrl+Z is
	// SIGTSTP (suspend), Ctrl+Y is readline yank. So forward the control byte to the
	// PTY instead of letting them fall through to the no-op default. (On macOS these
	// are bound to Cmd, so plain Ctrl+A/Z/Y arrive as CustomShortcut and already work
	// via keyNameToControlByte; this case is what brings Linux/Windows in line.)
	case *fyne.ShortcutSelectAll:
		dprintf("SelectAll shortcut (Ctrl+A) -> PTY 0x01\n")
		t.sendInput([]byte{0x01}) // Ctrl+A
		if t.screen != nil {
			t.screen.InvalidateCache()
		}
		t.triggerImmediateRedraw()

	case *fyne.ShortcutUndo:
		dprintf("Undo shortcut (Ctrl+Z) -> PTY 0x1A\n")
		t.sendInput([]byte{0x1A}) // Ctrl+Z (SIGTSTP)
		if t.screen != nil {
			t.screen.InvalidateCache()
		}
		t.triggerImmediateRedraw()

	case *fyne.ShortcutRedo:
		dprintf("Redo shortcut (Ctrl+Y) -> PTY 0x19\n")
		t.sendInput([]byte{0x19}) // Ctrl+Y
		if t.screen != nil {
			t.screen.InvalidateCache()
		}
		t.triggerImmediateRedraw()

	default:
		dprintf("Unhandled shortcut type: %T\n", shortcut)
		// Call the embedded shortcut handler for other shortcuts
		t.ShortcutHandler.TypedShortcut(shortcut)
	}

	t.updatePending.Store(true)
}

// Add this helper method for Alt key character mapping
func (t *NativeTerminalWidget) keyNameToChar(keyName fyne.KeyName) byte {
	switch keyName {
	case fyne.KeyA:
		return 'a'
	case fyne.KeyB:
		return 'b'
	case fyne.KeyC:
		return 'c'
	case fyne.KeyD:
		return 'd'
	case fyne.KeyE:
		return 'e'
	case fyne.KeyF:
		return 'f'
	case fyne.KeyG:
		return 'g'
	case fyne.KeyH:
		return 'h'
	case fyne.KeyI:
		return 'i'
	case fyne.KeyJ:
		return 'j'
	case fyne.KeyK:
		return 'k'
	case fyne.KeyL:
		return 'l'
	case fyne.KeyM:
		return 'm'
	case fyne.KeyN:
		return 'n'
	case fyne.KeyO:
		return 'o'
	case fyne.KeyP:
		return 'p'
	case fyne.KeyQ:
		return 'q'
	case fyne.KeyR:
		return 'r'
	case fyne.KeyS:
		return 's'
	case fyne.KeyT:
		return 't'
	case fyne.KeyU:
		return 'u'
	case fyne.KeyV:
		return 'v'
	case fyne.KeyW:
		return 'w'
	case fyne.KeyX:
		return 'x'
	case fyne.KeyY:
		return 'y'
	case fyne.KeyZ:
		return 'z'
	case fyne.Key0:
		return '0'
	case fyne.Key1:
		return '1'
	case fyne.Key2:
		return '2'
	case fyne.Key3:
		return '3'
	case fyne.Key4:
		return '4'
	case fyne.Key5:
		return '5'
	case fyne.Key6:
		return '6'
	case fyne.Key7:
		return '7'
	case fyne.Key8:
		return '8'
	case fyne.Key9:
		return '9'
	default:
		return 0
	}
}

// ADD: keyNameToControlByte method
func (t *NativeTerminalWidget) keyNameToControlByte(keyName fyne.KeyName) byte {
	switch keyName {
	case fyne.KeyA:
		return 0x01 // Ctrl+A
	case fyne.KeyB:
		return 0x02 // Ctrl+B
	case fyne.KeyC:
		return 0x03 // Ctrl+C
	case fyne.KeyD:
		return 0x04 // Ctrl+D
	case fyne.KeyE:
		return 0x05 // Ctrl+E
	case fyne.KeyF:
		return 0x06 // Ctrl+F
	case fyne.KeyG:
		return 0x07 // Ctrl+G
	case fyne.KeyH:
		return 0x08 // Ctrl+H
	case fyne.KeyI:
		return 0x09 // Ctrl+I
	case fyne.KeyJ:
		return 0x0A // Ctrl+J
	case fyne.KeyK:
		return 0x0B // Ctrl+K
	case fyne.KeyL:
		return 0x0C // Ctrl+L
	case fyne.KeyM:
		return 0x0D // Ctrl+M
	case fyne.KeyN:
		return 0x0E // Ctrl+N
	case fyne.KeyO:
		return 0x0F // Ctrl+O
	case fyne.KeyP:
		return 0x10 // Ctrl+P
	case fyne.KeyQ:
		return 0x11 // Ctrl+Q
	case fyne.KeyR:
		return 0x12 // Ctrl+R
	case fyne.KeyS:
		return 0x13 // Ctrl+S
	case fyne.KeyT:
		return 0x14 // Ctrl+T
	case fyne.KeyU:
		return 0x15 // Ctrl+U
	case fyne.KeyV:
		return 0x16 // Ctrl+V
	case fyne.KeyW:
		return 0x17 // Ctrl+W
	case fyne.KeyX:
		return 0x18 // Ctrl+X
	case fyne.KeyY:
		return 0x19 // Ctrl+Y
	case fyne.KeyZ:
		return 0x1A // Ctrl+Z
	default:
		return 0
	}
}

// ADD: triggerImmediateRedraw method (removed - already exists in terminal_events.go)

// ADD: Missing unified history methods (removed - already exist in other files)

// CreateRenderer with unified scroll container
func (t *NativeTerminalWidget) CreateRenderer() fyne.WidgetRenderer {
	log.Printf("CreateRenderer: Creating unified renderer with enhanced scroll container")

	// Create the scroll container and scrollbar ONCE and reuse them. Fyne can call
	// CreateRenderer more than once for the same widget; rebuilding these every
	// time spawned a second VirtualScrollbar while updateUnifiedScrollBar kept
	// calling SetState on whichever instance t.scrollbar last pointed at - so the
	// on-screen thumb (a now-orphaned instance) never received state updates and
	// stayed frozen at its initial full/inactive size. Singletons keep the
	// rendered scrollbar and the one we drive as the same object.
	if t.scroll == nil {
		t.scroll = NewHybridScrollContainer(t)
		t.scroll.OptimizeForVirtualScrolling()
	}
	if t.scrollbar == nil {
		// Draggable scrollbar for the virtual scrollback, pinned to the right edge.
		// It drives gopyte history scrolling via ScrollToFraction; the render loop
		// keeps its thumb in sync. Border keeps its mouse events cleanly separate
		// from the terminal's (unlike a stacked overlay).
		t.scrollbar = NewVirtualScrollbar(t.ScrollToFraction)
	}
	if t.find == nil {
		// The find bar occupies the Border's top slot and starts hidden, so it
		// takes no vertical space until Ctrl+Shift+F opens it.
		t.find = newFindController(t)
	}
	content := container.NewBorder(t.find.bar, nil, nil, t.scrollbar, t.scroll)

	return &unifiedTerminalRenderer{
		widget:  t,
		scroll:  t.scroll,
		content: content,
	}
}

type unifiedTerminalRenderer struct {
	widget  *NativeTerminalWidget
	scroll  *HybridScrollContainer
	content fyne.CanvasObject
}

// Ensure we implement all required fyne.WidgetRenderer methods
func (r *unifiedTerminalRenderer) Layout(size fyne.Size) {
	r.content.Resize(size)

	widget := r.widget
	// The scrollbar occupies a fixed gutter on the right; exclude it so the
	// computed column count matches the grid's actual drawable width.
	cols, rows := widget.CalculateTerminalSize(size.Width-vScrollbarWidth, size.Height)

	widget.mutex.RLock()
	currentCols, currentRows := widget.cols, widget.rows
	needsUpdate := cols != currentCols || rows != currentRows
	widget.mutex.RUnlock()

	if needsUpdate {
		log.Printf("Layout: Unified terminal resize from %dx%d to %dx%d",
			currentCols, currentRows, cols, rows)

		// Same width the decision above was made on. Passing size.Width here
		// instead made the two disagree by exactly the gutter: Layout decided
		// "153 columns" and then performResize computed 155 from the full
		// width, so the widget told the far end it had two more columns than
		// the grid can actually draw, and the last two columns of every wide
		// line landed under the scrollbar.
		widget.handleResizeUnified(size.Width-vScrollbarWidth, size.Height)
	}
}

func (r *unifiedTerminalRenderer) MinSize() fyne.Size {
	return fyne.NewSize(400, 300)
}

func (r *unifiedTerminalRenderer) Refresh() {
	if r.content != nil {
		r.content.Refresh()
	}
}

func (r *unifiedTerminalRenderer) Objects() []fyne.CanvasObject {
	if r.content != nil {
		return []fyne.CanvasObject{r.content}
	}
	return []fyne.CanvasObject{}
}

func (r *unifiedTerminalRenderer) Destroy() {
	// Clean up any resources if needed
}

// ENHANCED DISPLAY PROCESSING - Works with unified history system
// performRedrawUnified snapshots the screen and paints it on the main thread.
// It returns false without painting when a previous paint is still in flight
// (the in-flight guard), so a frame that paints slower than the tick interval
// cannot stack repaints behind itself. Without this, btop-class output keeps
// updatePending set across a whole refresh burst, so the 60fps ticker queues
// dozens of full-screen paints per burst; they drain slower than they arrive
// and the backlog compounds to unresponsive. fyne.Do already marshals onto the
// main thread, so the previous goroutine wrapper only hid that growth.
func (t *NativeTerminalWidget) performRedrawUnified() bool {
	// The guard was disabled by a DIAGNOSTIC BISECT that asked whether it was
	// wedging the alt->main transition on btop's q-exit. It is reinstated here
	// in the defer-release form the bisect note called for, plus the stale
	// release described on redrawStarted -- which answers the bisect's own
	// worst case directly: even if a paint is lost, the widget recovers on the
	// next tick instead of never painting again.
	if !t.claimRedraw() {
		return false
	}

	f := t.snapshotFrame()

	fyne.Do(func() {
		// Registered FIRST so it runs LAST: the recover below stops the
		// panic, and the guard is released after that, so a crashing render
		// cannot leave the widget permanently unpaintable.
		defer t.redrawing.Store(false)

		// Log (but don't swallow the consequences of) a render panic, so a
		// crashing alt->main transition is visible instead of silent.
		defer func() {
			if r := recover(); r != nil {
				dlogf("performRedrawUnified: RENDER PANIC (alternate=%v): %v", f.isAlternate, r)
			}
		}()
		if f.isAlternate {
			t.renderAlternateScreenUnified(f)
		} else {
			// shouldAutoScroll was !f.inHistoryMode, and inHistoryMode came
			// from the pty manager -- nil on every non-local session, so this
			// has always been true in practice. Scrollback is handled by
			// f.viewingHist, which is read from the screen and actually wired.
			t.renderNormalModeUnified(f, true)
		}
	})
	return true
}

// redrawStaleAfter is how long a dispatched paint may be in flight before the
// guard assumes it was lost and lets another through. It is deliberately much
// larger than the tick interval: the point is to bound a permanent wedge, not
// to second-guess a slow frame. A frame that legitimately takes this long has
// a different problem.
const redrawStaleAfter = 2 * time.Second

// claimRedraw takes the in-flight guard, or reclaims it from a paint that was
// dispatched long enough ago to be considered lost. It reports whether the
// caller may dispatch.
func (t *NativeTerminalWidget) claimRedraw() bool {
	if t.redrawing.CompareAndSwap(false, true) {
		t.redrawStarted.Store(time.Now().UnixNano())
		return true
	}

	started := t.redrawStarted.Load()
	if started == 0 || time.Since(time.Unix(0, started)) < redrawStaleAfter {
		return false
	}

	// Reclaim. The lost paint's own release may still land later and clear a
	// flag this dispatch owns; the cost of that is one extra paint, which is
	// the behaviour with no guard at all and is why it is not worth a
	// generation counter.
	dlogf("performRedrawUnified: reclaiming a paint in flight for %s", time.Since(time.Unix(0, started)))
	t.redrawStarted.Store(time.Now().UnixNano())
	return true
}

// frame is an immutable snapshot of everything the paint path needs from the
// screen model. It exists because the render closure runs on the GL thread via
// fyne.Do, long after performRedrawUnified released the lock: any t.screen.*
// call inside that closure is an unsynchronized read of a model the SSH read
// goroutine is actively writing (confirmed by -race: GetCursor in
// renderNormalModeUnified vs CarriageReturn under Stream.Feed in readLoop).
//
// The rule this encodes: the paint closure touches this struct and Fyne widgets,
// nothing else. If a render helper needs a new piece of screen state, add a
// field here and fill it in snapshotFrame - do not reach back into t.screen.
type frame struct {
	lines         []string
	attrs         [][]gopyte.Attributes
	cursorX       int
	cursorY       int
	isAlternate   bool
	viewingHist   bool
	histPos       int
	maxHistPos    int
	viewportStart int
	totalLines    int
}

// snapshotFrame captures the whole model-side read set in one locked block, so
// the paint that follows is a pure function of the snapshot. Every field is read
// under the same lock acquisition, which also makes the frame internally
// consistent - previously the cursor could be read several milliseconds after
// the lines it was supposed to sit in.
func (t *NativeTerminalWidget) snapshotFrame() frame {
	t.mutex.RLock()
	defer t.mutex.RUnlock()

	// One acquisition of the model lock, inside gopyte. Reading these fields
	// through separate exported getters would let a feed land between them.
	snap := t.screen.Snapshot()

	return frame{
		lines:         snap.Lines,
		attrs:         snap.Attrs,
		cursorX:       snap.CursorX,
		cursorY:       snap.CursorY,
		isAlternate:   snap.UsingAlternate,
		viewingHist:   snap.ViewingHistory,
		histPos:       snap.HistoryPos,
		maxHistPos:    snap.MaxHistoryPos,
		viewportStart: snap.ViewportStart,
		totalLines:    snap.TotalContentLines,
	}
}

func (t *NativeTerminalWidget) renderNormalModeUnified(f frame, shouldAutoScroll bool) {
	allLines, allAttrs := f.lines, f.attrs

	dlogf("NORMAL (%s): Rendering with unified virtual scrolling, lines=%d, autoScroll=%v",
		runtime.GOOS, len(allLines), shouldAutoScroll)

	if len(allLines) == 0 {
		t.textGrid.SetText("")
		return
	}

	// Calculate viewport from the snapshot, not from live screen state.
	viewport := t.viewportFromFrame(f)

	// Size TextGrid to viewport, in the grid's OWN cell units rather than the
	// fontSize-ratio estimate (see gridCellSize).
	//
	// This looks like dead work -- container.Scroll resizes the content to
	// MinSize().Max(size) on every layout and overwrites it -- and it was
	// removed on exactly that reasoning. It is NOT dead: it is what marks the
	// canvas dirty each frame. Without it the terminal painted once and then
	// froze, with input still reaching the device and nothing appearing on
	// screen. The granular SetCell/refreshCell path in setStyledRows is not
	// enough on its own.
	gcw, gch := t.gridCellSize()
	viewportSize := fyne.NewSize(
		float32(t.cols)*gcw,
		float32(viewport.visibleLines)*gch,
	)

	currentSize := t.textGrid.Size()
	if currentSize.Width != viewportSize.Width || currentSize.Height != viewportSize.Height {
		t.textGrid.Resize(viewportSize)
		dlogf("NORMAL (%s): Resized viewport to %dx%d",
			runtime.GOOS, t.cols, viewport.visibleLines)
	}

	// Extract visible content from unified system
	visibleLines := t.extractUnifiedVisibleContent(allLines, viewport)
	visibleAttrs := t.extractUnifiedVisibleAttributes(allAttrs, viewport)

	// Handle cursor positioning
	cursorX, cursorY := f.cursorX, f.cursorY
	adjustedCursorY := t.adjustUnifiedCursor(cursorX, cursorY, viewport, len(allLines), f.viewingHist)

	// Place cursor if visible and not in history mode
	if adjustedCursorY >= 0 && adjustedCursorY < len(visibleLines) &&
		cursorX >= 0 && cursorX < t.cols && !f.viewingHist {
		t.placeCursorInLineFast(&visibleLines[adjustedCursorY], cursorX)
		dlogf("NORMAL (%s): Cursor at (%d,%d) in viewport", runtime.GOOS, cursorX, adjustedCursorY)
	}

	// Update only changed cells (see setStyledRows) instead of SetText +
	// applyColorsToTextGrid, which repainted every row widget and flashed colored
	// backgrounds. This is the live render path (the 16ms unified processor).
	var sel *selRange
	if t.selection != nil {
		topAbs := f.viewportStart + viewport.scrollOffset
		sel = t.selection.toRange(viewport, topAbs)
	}
	t.setStyledRows(visibleLines, visibleAttrs, sel)
	// Backgrounds (SGR + selection) are painted on the TextGrid cells themselves;
	// the overlay below is kept in sync but does not composite under the per-tab
	// font-size theme override, so the grid is the source of truth on screen.
	t.bgLayer.Update(visibleAttrs, sel)
	// Update scroll bar position
	t.updateUnifiedScrollBar(f, viewport)

	dlogf("NORMAL (%s): Rendered viewport lines %d-%d of %d total",
		runtime.GOOS, viewport.scrollOffset, viewport.scrollOffset+viewport.visibleLines-1, len(allLines))
}

// UNIFIED VIEWPORT CALCULATIONS
// calculateUnifiedViewport is the live-state entry point, kept for the callers
// that are NOT on the paint path (find, selection, the scroll handler). It reads
// the screen directly and then defers to the pure core below.
//
// The paint path must use viewportFromFrame instead: calling this from inside
// the fyne.Do closure is what put three unsynchronized screen reads on the GL
// thread (IsViewingHistory / GetHistoryPos / GetMaxHistoryPos).
func (t *NativeTerminalWidget) calculateUnifiedViewport(allLines []string) VirtualScrollState {
	var viewingHist bool
	var pos, total int
	if t.screen != nil {
		viewingHist = t.screen.IsViewingHistory()
		if viewingHist {
			pos, total = t.screen.GetHistoryPos(), t.screen.GetMaxHistoryPos()
		}
	}
	return t.calculateViewport(len(allLines), viewingHist, pos, total)
}

// viewportFromFrame is the paint path's entry point: same arithmetic, sourced
// entirely from the snapshot.
func (t *NativeTerminalWidget) viewportFromFrame(f frame) VirtualScrollState {
	return t.calculateViewport(len(f.lines), f.viewingHist, f.histPos, f.maxHistPos)
}

// calculateViewport is the pure core - no screen access, so it is safe to call
// from either thread with values captured wherever the caller got them.
func (t *NativeTerminalWidget) calculateViewport(totalLines int, viewingHist bool, pos, total int) VirtualScrollState {
	visibleLines := t.rows
	if visibleLines <= 0 {
		visibleLines = 24
	}

	var scrollOffset int

	// The engine's scrollback position is the only source here. TetherSSH also
	// had IsInHistoryModeUnified()/GetHistoryPosition(), which read the pty
	// manager and were nil/zero on any remote session; they are gone. The
	// history branch below maps position -> scrollOffset across the full range,
	// so the viewport (and the scrollbar mirroring it) reaches the true top and
	// bottom.
	if viewingHist {
		// UNIFIED HISTORY MODE CALCULATION
		dlogf("calculateUnifiedViewport (%s): HISTORY MODE - pos=%d, total=%d, totalLines=%d",
			runtime.GOOS, pos, total, totalLines)

		if totalLines <= visibleLines {
			scrollOffset = 0
		} else {
			maxScrollOffset := totalLines - visibleLines

			if total > 0 {
				// Map history position to scroll offset
				scrollOffset = maxScrollOffset - ((pos * maxScrollOffset) / total)
			} else {
				scrollOffset = maxScrollOffset
			}

			// Handle maximum position (top of history)
			if pos >= total {
				scrollOffset = 0
			}

			// Ensure bounds
			if scrollOffset < 0 {
				scrollOffset = 0
			}
			if scrollOffset > maxScrollOffset {
				scrollOffset = maxScrollOffset
			}
		}

		dlogf("calculateUnifiedViewport (%s): HISTORY - pos=%d/%d -> scrollOffset=%d",
			runtime.GOOS, pos, total, scrollOffset)
	} else {
		// Normal mode: show bottom (latest output)
		if totalLines <= visibleLines {
			scrollOffset = 0
		} else {
			scrollOffset = totalLines - visibleLines
		}

		dlogf("calculateUnifiedViewport (%s): NORMAL MODE - scrollOffset=%d",
			runtime.GOOS, scrollOffset)
	}

	// Calculate maximum scroll
	maxScroll := totalLines - visibleLines
	if maxScroll < 0 {
		maxScroll = 0
	}

	// Final bounds check
	if scrollOffset > maxScroll {
		scrollOffset = maxScroll
	}
	if scrollOffset < 0 {
		scrollOffset = 0
	}

	return VirtualScrollState{
		totalLines:    totalLines,
		visibleLines:  visibleLines,
		scrollOffset:  scrollOffset,
		maxScroll:     maxScroll,
		contentHeight: float32(totalLines) * t.cellHeight(),
	}
}

func (t *NativeTerminalWidget) extractUnifiedVisibleContent(allLines []string, viewport VirtualScrollState) []string {
	visibleContent := make([]string, viewport.visibleLines)

	for i := 0; i < viewport.visibleLines; i++ {
		lineIndex := viewport.scrollOffset + i
		if lineIndex < len(allLines) {
			visibleContent[i] = t.padLineToWidth(allLines[lineIndex])
		} else {
			visibleContent[i] = t.padLineToWidth("")
		}
	}

	return visibleContent
}

func (t *NativeTerminalWidget) extractUnifiedVisibleAttributes(allAttrs [][]gopyte.Attributes, viewport VirtualScrollState) [][]gopyte.Attributes {
	if len(allAttrs) == 0 {
		return [][]gopyte.Attributes{}
	}

	visibleAttrs := make([][]gopyte.Attributes, viewport.visibleLines)

	for i := 0; i < viewport.visibleLines; i++ {
		lineIndex := viewport.scrollOffset + i
		if lineIndex < len(allAttrs) {
			visibleAttrs[i] = allAttrs[lineIndex]
		}
	}

	return visibleAttrs
}

// adjustUnifiedCursor takes viewingHist from the frame rather than reading the
// screen: it runs inside the paint closure, where a live read is both a race
// and a chance to disagree with the lines already captured.
func (t *NativeTerminalWidget) adjustUnifiedCursor(cursorX, cursorY int, viewport VirtualScrollState, totalLines int, viewingHist bool) int {
	// Don't show cursor in history mode (engine state, valid for SSH).
	if viewingHist {
		return -1
	}

	// Calculate cursor position in viewport
	if totalLines <= viewport.visibleLines {
		return cursorY
	} else {
		actualCursorLine := totalLines - viewport.visibleLines + cursorY
		if actualCursorLine >= viewport.scrollOffset && actualCursorLine < viewport.scrollOffset+viewport.visibleLines {
			return actualCursorLine - viewport.scrollOffset
		}
		return -1 // Cursor not visible
	}
}

// UTILITY METHODS
func (t *NativeTerminalWidget) padLineToWidth(line string) string {
	runes := []rune(line)
	if len(runes) < t.cols {
		padding := strings.Repeat(" ", t.cols-len(runes))
		return line + padding
	} else if len(runes) > t.cols {
		return string(runes[:t.cols])
	}
	return line
}

func (t *NativeTerminalWidget) updateUnifiedScrollBar(f frame, viewport VirtualScrollState) {
	// Drive the draggable grabber from the TRUE scrollback geometry, not the
	// windowed VirtualScrollState. scrollOffset/maxScroll/totalLines are relative
	// to the currently displayed window, which changes size under progressive
	// history paging - using them made the thumb stretch (height = visible/window)
	// and mis-track (position within window, not within full scrollback).
	//
	// Instead: absolute viewport-top = GetViewportStart()+scrollOffset (the same
	// stable coordinate the selection layer uses), total = history+screen rows.
	// Both the thumb height and position are then constant-referenced, so the
	// thumb keeps a fixed size and slides in lockstep with lines scrolled.
	if t.scrollbar != nil {
		total := f.totalLines
		visible := viewport.visibleLines
		maxTop := total - visible // = history size; lines you can scroll past
		active := !f.isAlternate && maxTop > 0 && total > 0
		var pos, thumb float32 = 1, 1
		if active {
			topAbs := f.viewportStart + viewport.scrollOffset
			pos = float32(topAbs) / float32(maxTop)
			thumb = float32(visible) / float32(total)
		}
		t.scrollbar.SetState(pos, thumb, active)
	}

	if t.scroll == nil {
		return
	}

	// A refresh is scheduled here; the container's scroll OFFSET is not touched.
	//
	// What used to live here assigned t.scroll.Scroll.Offset -- on the EMBEDDED
	// Scroll, bypassing every guard on HybridScrollContainer -- from a goroutine
	// 5ms after each paint. The value came from contentHeight (totalLines *
	// cellHeight), which exceeds the container as soon as there is any
	// scrollback, so it fired on every paint of every session with history.
	// Fyne then clamped it in refreshBars to the only room available: the
	// sentinel row's leftover, between 1px and a full cell. That left the grid
	// sitting a few pixels high, and once anything else zeroed the offset, a
	// visible jump down and back on every click as this put it back.
	//
	// The Refresh, however, is load-bearing: it is what marks the canvas dirty
	// after a frame. Removing the whole block froze the display -- input still
	// reached the device, nothing appeared on screen -- which is indistinguishable
	// from losing the keyboard. So the refresh stays and the offset does not.
	go func() {
		time.Sleep(5 * time.Millisecond)
		fyne.Do(func() {
			if t.scroll == nil {
				return
			}
			t.scroll.pinOffset()
			t.scroll.Scroll.Refresh()
		})
	}()
}

// SetTargetLabel records which device this terminal is connected to, for the
// places that have to name it. Called from ApplySession, which is the one
// place every host configures a session from a node.
func (t *NativeTerminalWidget) SetTargetLabel(label string) {
	t.mutex.Lock()
	t.targetLabel = label
	t.mutex.Unlock()
}

// TargetLabel is the device name, falling back to the window title so a
// terminal that was never configured from a node still answers something.
func (t *NativeTerminalWidget) TargetLabel() string {
	t.mutex.RLock()
	defer t.mutex.RUnlock()
	if t.targetLabel != "" {
		return t.targetLabel
	}
	return t.title
}

// SetPasteWarnLines sets the line count at or above which a paste asks for
// confirmation. Zero turns the question off for this terminal.
func (t *NativeTerminalWidget) SetPasteWarnLines(n int) {
	t.mutex.Lock()
	t.pasteWarnLines = n
	t.mutex.Unlock()
}

// SetPasteRememberFunc installs the hook that persists a pacing override
// chosen in the paste confirmation. See the field comment: nil hides the
// checkbox rather than offering a promise nothing keeps.
func (t *NativeTerminalWidget) SetPasteRememberFunc(fn func(lineDelayMs, consoleBaud int)) {
	t.mutex.Lock()
	t.pasteRemember = fn
	t.mutex.Unlock()
}

// SetPastePacing installs a pacing pair on this terminal for subsequent
// pastes. The dialog calls it when an override is remembered, so the tab that
// is already open behaves like the saved session immediately.
func (t *NativeTerminalWidget) SetPastePacing(lineDelayMs, consoleBaud int) {
	t.mutex.Lock()
	t.pasteLineDelayMs = lineDelayMs
	t.pasteConsoleBaud = consoleBaud
	t.mutex.Unlock()
}

// PUBLIC API - Enhanced unified methods
func (t *NativeTerminalWidget) GetTitle() string {
	t.mutex.RLock()
	defer t.mutex.RUnlock()
	return t.title
}

func (t *NativeTerminalWidget) GetContext() context.Context {
	return t.ctx
}

// extractWindowTitle records an OSC 0 title set by the remote.
//
// NOT WIRED. Nothing calls this yet: there is no tab strip or window chrome to
// put a title in, so parsing one would write to a field nobody reads. GetTitle
// is the other half, equally unwired.
//
// Kept deliberately rather than deleted as dead code. It is unfinished work
// with a known trigger — the tabbed shell — not residue of the port, and the
// escape-sequence handling is the part that would have to be rediscovered.
func (t *NativeTerminalWidget) extractWindowTitle(data string) {
	start := strings.Index(data, "\x1b]0;")
	if start < 0 {
		return
	}
	start += 4 // past "\x1b]0;"

	// The title ends at BEL or at the start of ST (ESC backslash).
	end := strings.IndexAny(data[start:], "\x07\x1b")
	if end < 0 {
		return
	}
	newTitle := data[start : start+end]
	if newTitle == t.title {
		return
	}
	t.title = newTitle
	log.Printf("TERMINAL: window title changed to: %s", newTitle)
}

func (t *NativeTerminalWidget) Clear() {
	// Cross-platform clear screen command
	t.sendInput([]byte("\x1b[2J\x1b[H"))
}

func (t *NativeTerminalWidget) GetHistorySize() int {
	if t.screen != nil {
		return t.screen.GetHistorySize()
	}
	return 0
}

func (t *NativeTerminalWidget) IsInHistoryMode() bool {
	if t.screen != nil {
		return t.screen.IsViewingHistory()
	}
	return false
}

func (t *NativeTerminalWidget) ScrollToTop() {
	if t.screen != nil {
		t.screen.ScrollToTop()
		t.updatePending.Store(true)
	}
}

// ScrollToFraction scrolls the virtual scrollback so the viewport top sits at
// the given fraction of the scrollable range (0 = oldest/top, 1 = live tail).
// It is the callback behind the draggable scrollbar.
//
// The mapping is done in ABSOLUTE buffer-line space (history+screen), the same
// coordinate the thumb is drawn from, so a given thumb travel corresponds 1:1
// to that many scrollback lines. We translate the absolute delta into the
// gopyte scroll calls the wheel uses. For very deep scrollback the line->view
// mapping can be slightly nonlinear, so a drag converges over a frame or two
// rather than landing exactly; the thumb re-syncs each frame from the real
// position.
func (t *NativeTerminalWidget) ScrollToFraction(frac float32) {
	if t.screen == nil || t.screen.IsUsingAlternate() {
		return
	}
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}

	allLines := t.screen.GetDisplay()
	vp := t.calculateUnifiedViewport(allLines)

	total := t.screen.GetTotalContentLines()
	maxTop := total - vp.visibleLines // absolute lines you can scroll past
	if maxTop <= 0 {
		return
	}

	targetTop := int(frac*float32(maxTop) + 0.5)
	if targetTop < 0 {
		targetTop = 0
	}
	if targetTop > maxTop {
		targetTop = maxTop
	}

	currentTop := t.screen.GetViewportStart() + vp.scrollOffset
	delta := targetTop - currentTop // +ve = move toward newer/bottom

	if delta > 0 {
		t.screen.ScrollDown(delta)
	} else if delta < 0 {
		t.screen.ScrollUp(-delta)
	}
	t.updatePending.Store(true)
}

func (t *NativeTerminalWidget) ScrollToBottom() {
	if t.screen != nil {
		t.screen.ScrollToBottom()
		t.updatePending.Store(true)
	}
}

func (t *NativeTerminalWidget) SetMaxHistoryLines(maxLines int) {
	if t.screen != nil {
		t.screen.SetMaxHistoryLines(maxLines)
		log.Printf("Set max history lines to %d", maxLines)
	}
}

func (t *NativeTerminalWidget) Close() {
	log.Printf("Closing unified terminal widget (%s)", runtime.GOOS)

	// Cancel context
	t.cancel()

	// Stop resize timer
	t.resizeMutex.Lock()
	if t.resizeTimer != nil {
		t.resizeTimer.Stop()
	}
	t.resizeMutex.Unlock()

}

// Implement the Tabbable interface to capture Tab key events
func (t *NativeTerminalWidget) AcceptsTab() bool {
	return true // This tells Fyne to send Tab key events to this widget
}

// ENHANCED UPDATE PROCESSOR - Same as before but with logging
func (t *NativeTerminalWidget) updateProcessor() {
	ticker := time.NewTicker(33 * time.Millisecond) // ~30 FPS
	defer ticker.Stop()

	var lastUpdateTime time.Time
	updateCooldown := 16 * time.Millisecond
	updateCount := 0

	log.Printf("Unified update processor started for %s", runtime.GOOS)

	for {
		select {
		case <-ticker.C:
			now := time.Now()
			// Test-and-clear BEFORE dispatching. The old order cleared the flag
			// after performRedrawUnified returned, so a chunk that arrived while
			// the snapshot was being taken set updatePending and then had it
			// wiped - one lost frame, and if it was the last chunk of a burst
			// there was no later tick to recover it. That is the "tail doesn't
			// render until I hit a key" path. Clearing first means such a chunk
			// re-arms the flag and paints on the next tick instead.
			if now.Sub(lastUpdateTime) >= updateCooldown && t.updatePending.CompareAndSwap(true, false) {
				lastUpdateTime = now
				if !t.performRedrawUnified() {
					// In-flight guard skipped the paint; re-arm so the latest
					// state still lands on a later tick.
					t.updatePending.Store(true)
					continue
				}
				updateCount++

				// Log occasionally for monitoring
				if updateCount%100 == 0 {
					log.Printf("Update processor (%s): %d redraws completed", runtime.GOOS, updateCount)
				}
			}
		case <-t.ctx.Done():
			log.Printf("Unified update processor stopping after %d updates", updateCount)
			return
		}
	}
}

// HELPER METHODS
func (t *NativeTerminalWidget) calculateCharDimensions() {
	// Platform-specific character dimension calculations
	switch runtime.GOOS {
	case "windows":
		// Windows tends to have slightly different font rendering
		t.charWidth = t.fontSize * 0.58
		t.charHeight = t.fontSize * 1.25
	case "darwin":
		// macOS font rendering
		t.charWidth = t.fontSize * 0.55
		t.charHeight = t.fontSize * 1.15
	default:
		// Linux and other Unix systems
		t.charWidth = t.fontSize * 0.56
		t.charHeight = t.fontSize * 1.22
	}

	log.Printf("Character dimensions (%s): %.2fx%.2f for fontSize %.1f",
		runtime.GOOS, t.charWidth, t.charHeight, t.fontSize)
}

func (t *NativeTerminalWidget) initializeTextGridSize() {
	cw, ch := t.gridCellSize()
	initialSize := fyne.NewSize(
		float32(t.cols)*cw,
		float32(t.rows)*ch,
	)
	t.textGrid.Resize(initialSize)

	log.Printf("Initial TextGrid size (%s): %.1fx%.1f for %dx%d terminal",
		runtime.GOOS, initialSize.Width, initialSize.Height, t.cols, t.rows)
}

func (t *NativeTerminalWidget) placeCursorInLineFast(line *string, cursorX int) {
	if line == nil || cursorX < 0 {
		return
	}

	currentLine := *line
	runes := []rune(currentLine)

	if cursorX < len(runes) {
		// Replace character at cursor position with block cursor
		runes[cursorX] = '█' // Block cursor character
		*line = string(runes)
	} else if cursorX < t.cols {
		// Extend line with spaces and place cursor
		padLen := cursorX - len(runes)
		if padLen > 0 {
			padding := strings.Repeat(" ", padLen)
			*line = currentLine + padding + "█"
		} else {
			*line = currentLine + "█"
		}
	}
}

// MISSING METHOD 3: handleResizeUnified (alias to existing method)
func (t *NativeTerminalWidget) handleResizeUnified(width, height float32) {
	// Call your existing handleResize method
	t.handleResize(width, height)
}

// ResyncSize recomputes the grid from the widget's CURRENT size and applies it
// immediately, cancelling any pending debounce.
//
// Normal resizes come from the renderer's Layout and are debounced by 150ms,
// which is right for a window being dragged: the far end gets one size, not
// forty. It is wrong for a terminal that has just been MOVED into another
// window. Canvas.SetContent lays new content out at its MINIMUM size first, so
// the widget briefly believes it is the renderer's 400x300 floor, and if the
// real window size does not arrive within the debounce the far end is told the
// wrong dimensions and a full-screen application redraws itself into a corner.
//
// A host that moves a terminal calls this once the window has reached its
// size. It is a no-op if the geometry has not actually changed.
func (t *NativeTerminalWidget) ResyncSize() {
	size := t.Size()
	width := size.Width - vScrollbarWidth
	if width <= 0 || size.Height <= 0 {
		return
	}

	t.resizeMutex.Lock()
	if t.resizeTimer != nil {
		t.resizeTimer.Stop()
		t.resizeTimer = nil
	}
	t.lastWidth, t.lastHeight = width, size.Height
	t.resizeMutex.Unlock()

	// Same width the renderer's Layout decides on -- the scrollbar gutter is
	// excluded there too, and the two must not disagree or the far end is
	// told it has columns the grid cannot draw.
	t.performResize(width, size.Height)
}

// brightenName maps a base ANSI color name to its bright_ variant. Bold text
// uses the bright palette entry rather than a per-channel nudge, so bold-black
// becomes a legible grey (bright_black) instead of near-black. Non-base names
// (already-bright, 256-color, "default", "") pass through unchanged.
func brightenName(name string) string {
	switch name {
	case "red", "green", "yellow", "blue", "magenta", "cyan", "white":
		return "bright_" + name
	case "brown": // gopyte's name for SGR 33 (yellow)
		return "bright_yellow"
	}
	return name
}

func (t *NativeTerminalWidget) renderAlternateScreenUnified(f frame) {
	allLines, allAttrs := f.lines, f.attrs

	dlogf("ALTERNATE (%s): Rendering full screen mode", runtime.GOOS)

	// DIAGNOSTIC: dump non-(width-1) cells so we can see the exact codepoint and
	// width gopyte assigned to the glyph that's corrupting (the "??"+box). Read
	// under the lock since the data goroutine writes the buffer.
	if debugEnabled {
		t.mutex.RLock()
		cells := t.screen.DebugScanCells()
		t.mutex.RUnlock()
		if len(cells) > 0 {
			total := len(cells)
			if len(cells) > 24 {
				cells = cells[:24]
			}
			dlogf("ALT SUSPECT CELLS (%d of %d): %s", len(cells), total, strings.Join(cells, " | "))
		}
	}

	// Size TextGrid to exact screen dimensions, in the grid's own cell units.
	// Load-bearing for repaint -- see the normal path above.
	altCW, altCH := t.gridCellSize()
	screenSize := fyne.NewSize(
		float32(t.cols)*altCW,
		float32(t.rows)*altCH,
	)

	currentSize := t.textGrid.Size()
	if currentSize.Width != screenSize.Width || currentSize.Height != screenSize.Height {
		t.textGrid.Resize(screenSize)
		dlogf("ALTERNATE (%s): Resized to %dx%d", runtime.GOOS, t.cols, t.rows)
	}

	// Display handling for alternate screen
	var displayLines []string
	var displayAttrs [][]gopyte.Attributes

	if len(allLines) == t.rows {
		// Perfect match - use exactly what app provides
		displayLines = make([]string, len(allLines))
		for i, line := range allLines {
			displayLines[i] = t.padLineToWidth(line)
		}
		displayAttrs = allAttrs
		dlogf("ALTERNATE: Using app's exact %d lines", len(allLines))

	} else if len(allLines) < t.rows {
		// App provided fewer lines - pad to screen height
		displayLines = make([]string, t.rows)
		displayAttrs = make([][]gopyte.Attributes, t.rows)

		for i := 0; i < t.rows; i++ {
			if i < len(allLines) {
				displayLines[i] = t.padLineToWidth(allLines[i])
				if i < len(allAttrs) {
					displayAttrs[i] = allAttrs[i]
				}
			} else {
				displayLines[i] = t.padLineToWidth("")
			}
		}
		dlogf("ALTERNATE: Using %d app lines, padded to %d", len(allLines), t.rows)

	} else {
		// App provided MORE lines than screen height
		startIdx := len(allLines) - t.rows
		displayLines = make([]string, t.rows)
		displayAttrs = make([][]gopyte.Attributes, t.rows)

		for i := 0; i < t.rows; i++ {
			displayLines[i] = t.padLineToWidth(allLines[startIdx+i])
			if startIdx+i < len(allAttrs) {
				displayAttrs[i] = allAttrs[startIdx+i]
			}
		}
		dlogf("ALTERNATE: Used last %d lines from %d total", t.rows, len(allLines))
	}

	// Get cursor position
	cursorX, cursorY := f.cursorX, f.cursorY

	// Place cursor exactly where app says it should be
	if cursorY >= 0 && cursorY < len(displayLines) && cursorX >= 0 && cursorX < t.cols {
		t.placeCursorInLineFast(&displayLines[cursorY], cursorX)
	}

	// Alternate-screen apps (vim/htop/btop) used to repaint here via SetText +
	// applyColorsToTextGrid, which rebuilt every row from a string and then ran
	// a top-level Refresh() - i.e. every cell repainted every frame, even the
	// ones that didn't change. That is the dominant cost behind the sluggish
	// btop/htop feel (worst on Windows, where each cell is its own GL draw).
	//
	// Route through setStyledRows instead, exactly like normal mode: it diffs
	// against the current grid and SetCell-updates only the cells that actually
	// changed, with no top-level Refresh. Full-screen apps still change a lot,
	// but rarely all 80x24 cells - static chrome (borders, labels, unchanged
	// rows, an idle status line) now costs nothing. The +1 sentinel row that
	// setStyledRows appends is what makes the bottom line (vim/htop status bar)
	// refreshable under Fyne's last-row guard; it sits past the grid height and
	// is clipped, so it never shows. Color (incl. bold/bright and the bold-black
	// htop fix) now lives in the shared cellColors, so this path renders the
	// same colors it did on the old applyColorsToTextGrid path.
	var sel *selRange
	viewport := t.viewportFromFrame(f)
	if t.selection != nil {
		topAbs := f.viewportStart + viewport.scrollOffset
		sel = t.selection.toRange(viewport, topAbs)
	}

	t.setStyledRows(displayLines, displayAttrs, sel)

	// Backgrounds are the bgLayer's job, exactly as in normal mode. The render
	// stack is baseBG -> bgLayer -> (transparent) textGrid, so SGR backgrounds,
	// reverse-video cells and the selection are drawn as rectangles BEHIND the
	// glyphs - which is most of what htop/btop/vim's UI actually is. Feeding the
	// real per-cell attributes here (not a selection-only zero grid, as the old
	// alt path did) is what makes those backgrounds paint, and bgLayer.Update's
	// canvas.Refresh doubles as this frame's paint flush. Without it the cells
	// updated by setStyledRows above never reach the screen and the app looks
	// blank/broken - the regression this restores.
	t.bgLayer.Update(displayAttrs, sel)

	// Mirror normal mode's tail. In alternate screen updateUnifiedScrollBar
	// resolves active=false (IsUsingAlternate), so it hides the thumb just like
	// the old explicit SetState(1,1,false) did, and its scroll refresh is the
	// backstop that lands the SetCell glyph updates - the same mechanism normal
	// mode relies on.
	t.updateUnifiedScrollBar(f, viewport)

	dlogf("ALTERNATE: Rendered %d lines", len(displayLines))
}

// updateAltScreenSelectionOverlay repaints the background overlay with the
// current selection while a full-screen app owns the terminal. It derives the
// viewport and absolute top line exactly as SelectionManager.viewportInfo does
// (GetDisplay -> calculateUnifiedViewport -> GetViewportStart), so the drawn
// rectangle is aligned with the anchor/focus that posToAbs recorded on
// mouse-down by construction, regardless of what those helpers return in
// alternate-screen mode.
//
// NOT WIRED. Selection under the alternate screen is on pfterm's manual
// checklist and has never been signed off, so this is half-finished work
// rather than superseded code — the reason it survives a dead-code cull.
func (t *NativeTerminalWidget) updateAltScreenSelectionOverlay(allLines []string) {
	if t.bgLayer == nil {
		return
	}

	var sel *selRange
	if t.selection != nil {
		viewport := t.calculateUnifiedViewport(allLines)
		topAbs := t.screen.GetViewportStart() + viewport.scrollOffset
		sel = t.selection.toRange(viewport, topAbs)
	}

	if sel == nil {
		// No selection: clear the overlay cheaply.
		t.bgLayer.Update(nil, nil)
		return
	}

	// Zero-value attrs grid sized to the visible window: every cell reports a
	// default (nil) background, so the overlay's run builder emits cells only
	// where the selection covers them - no SGR backgrounds are redrawn here.
	rows := t.rows
	if rows <= 0 {
		rows = len(allLines)
	}
	cols := t.cols
	selAttrs := make([][]gopyte.Attributes, rows)
	for i := range selAttrs {
		selAttrs[i] = make([]gopyte.Attributes, cols)
	}
	t.bgLayer.Update(selAttrs, sel)
}

func (t *NativeTerminalWidget) DragEnd() {
	dprintf("DragEnd called\n")

	if t.selection != nil && t.selection.IsSelecting() {
		// Finalize selection
		t.selection.HandleMouseUp(&desktop.MouseEvent{
			Button: desktop.MouseButtonPrimary,
		})
	}
}

// Make sure you have Dragged defined correctly
func (t *NativeTerminalWidget) Dragged(event *fyne.DragEvent) {
	dprintf("Dragged to %.1f,%.1f\n", event.Position.X, event.Position.Y)

	if t.isSelecting {
		t.selectionEnd = event.Position

		if t.selection != nil {
			t.selection.HandleDrag(event.AbsolutePosition, event.Position)
		}
	}
}

func (t *NativeTerminalWidget) Tapped(event *fyne.PointEvent) {
	dprintf("Tapped at %.1f,%.1f (was selecting: %v)\n",
		event.Position.X, event.Position.Y, t.isSelecting)

	if t.isSelecting {
		// Complete the selection
		t.isSelecting = false
		if t.selection != nil {
			t.selection.HandleMouseUp(&desktop.MouseEvent{
				Button: desktop.MouseButtonPrimary,
			})
		}
	}
}
func (t *NativeTerminalWidget) findWordBoundaries(line string, col int) (start, end int) {
	runes := []rune(line)
	if col >= len(runes) {
		return col, col
	}

	// Word delimiters
	isDelimiter := func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\\' || r == '/' ||
			r == '(' || r == ')' || r == '[' || r == ']' ||
			r == '{' || r == '}' || r == '<' || r == '>' ||
			r == '"' || r == '\'' || r == ',' || r == ';' ||
			r == ':' || r == '|' || r == '.' || r == '-'
	}

	// Find start of word
	start = col
	for start > 0 && !isDelimiter(runes[start-1]) {
		start--
	}

	// Find end of word
	end = col
	for end < len(runes) && !isDelimiter(runes[end]) {
		end++
	}

	return start, end
}

// gridCellSize returns the ACTUAL on-screen size of one character cell, read
// back from the TextGrid itself. The visible glyphs are a
// widget.TextGrid, which renders at the app theme's text size (NOT the
// terminal's fontSize), so this measured value - not the fontSize-derived
// charWidth/charHeight - is the single source of truth for mapping pixels to
// cells. Mouse hit-testing AND the selection overlay must both use it; if one
// uses this and the other uses charHeight, a click and its highlight land on
// different rows whenever the theme text size and fontSize disagree (which is
// exactly what a denser app theme introduced). Falls back to the fontSize ratio
// before the grid has any rows.
func (t *NativeTerminalWidget) gridCellSize() (cw, ch float32) {
	// FIRST CHOICE: read the cell back OUT of the grid rather than predicting it.
	// PositionForCursorLocation(row, col) returns (col*cellSize.Width,
	// row*cellSize.Height), so (1,1) IS the cell -- the exact divisor
	// CursorLocationForPosition uses to turn a pixel into a row. A readback cannot
	// disagree with the grid; a recomputation can, and when it does, nothing in
	// the stack is able to report the disagreement because each side is
	// internally consistent. Measured on macOS at fontSize 17: the grid's cell is
	// 10.00x20.00 while the fontSize-ratio estimate says 9.35x19.55 - a 3.3% error
	// that compounds into a full row within 20 lines.
	if rcw, rch, ok := t.gridCellSizeReadback(); ok {
		return rcw, rch
	}

	// FALLBACK, used only before Fyne has built the grid's renderer: compute the
	// cell the SAME way widget.TextGrid does internally - MeasureText("M",
	// fontSize, Monospace), width and height rounded. We do NOT read it from
	// textGrid.MinSize(), because when the terminal is inside a
	// container.NewThemeOverride the grid RENDERS at the override's text size
	// while MinSize can still resolve against the global app theme - the two
	// diverge and selection/overlay land nowhere near the glyphs.
	if t.fontSize > 0 {
		sz := fyne.MeasureText("M", t.fontSize, fyne.TextStyle{Monospace: true})
		cw = float32(math.Round(float64(sz.Width)))
		ch = float32(math.Round(float64(sz.Height)))
		if cw > 0 && ch > 0 {
			return cw, ch
		}
	}
	return t.charWidth, t.charHeight
}

// cellHeight is gridCellSize's height, for the viewport calculators. They run on
// both the paint and the event path, so they must not disagree with the hit test
// about how tall a row is.
func (t *NativeTerminalWidget) cellHeight() float32 {
	_, ch := t.gridCellSize()
	return ch
}

// gridCellSizeReadback asks the TextGrid for its own cell size.
//
// PositionForCursorLocation dereferences the grid's renderer-owned content,
// which does not exist until Fyne has created the renderer -- and there is no
// exported way to ask whether it has. So the read is guarded rather than
// predicted: callers that run before first paint (construction, toolkit-free
// tests) get ok=false and keep their arithmetic, and everyone after first paint
// gets the authoritative value.
func (t *NativeTerminalWidget) gridCellSizeReadback() (cw, ch float32, ok bool) {
	if t.textGrid == nil {
		return 0, 0, false
	}
	defer func() {
		if recover() != nil {
			cw, ch, ok = 0, 0, false
		}
	}()

	p := t.textGrid.PositionForCursorLocation(1, 1)
	if p.X <= 0 || p.Y <= 0 {
		return 0, 0, false
	}
	return p.X, p.Y, true
}

// gridCellAt maps a position in the WIDGET's coordinate space to a grid cell,
// by asking the TextGrid rather than recomputing what it did.
//
// # Why ask instead of calculate
//
// gridCellSize() reimplements Fyne's own cell arithmetic. A reimplementation
// of someone else's expression can only ever agree with it or be wrong about
// it, and when it is wrong the symptom is that clicks land one row off the
// glyphs -- with nothing in the stack able to report a disagreement, because
// each side is internally consistent.
//
// widget.TextGrid.CursorLocationForPosition is exported (Fyne v2.6) and uses
// the grid's OWN cellSize and its own scroll offset. It is the authority.
//
// # The coordinate conversion is measured, not assumed
//
// Mouse events arrive relative to this widget; the grid sits inside a Border
// (find bar on top, scrollbar on the right) and a scroll container, so its
// origin is NOT this widget's origin. That offset is exactly the quantity a
// hand-rolled mapping has no way to know it is missing, and it is available
// from the driver for the asking.
//
// Reports false when there is no canvas yet (constructed but never shown, and
// every toolkit-free test), so callers keep their arithmetic as the fallback
// rather than silently mapping everything to cell 0,0.
func (t *NativeTerminalWidget) gridCellAt(pos fyne.Position) (row, col int, ok bool) {
	origin, ok := t.gridOrigin()
	if !ok {
		return 0, 0, false
	}
	row, col = t.textGrid.CursorLocationForPosition(pos.Subtract(origin))
	return row, col, true
}

// gridCellAtAbs maps a CANVAS-ABSOLUTE position to a grid cell.
//
// This is the mapping every entry point should use. gridCellAt subtracts one
// driver lookup from another (grid-relative-to-terminal) and then applies that
// to a position whose space depends on WHICH widget the driver happened to
// deliver the event to -- mouse events land on the scroll container, taps land
// on the terminal. Getting that pairing wrong displaces the hit test by the
// whole window chrome, and nothing reports it.
//
// Absolute positions have no such ambiguity: every fyne.PointEvent carries one,
// and the grid's own absolute position is the only other quantity needed. One
// lookup, one subtraction, no assumption about the delivery path.
//
// Reports false if the result lands outside the grid by more than a cell, which
// is what a failed lookup looks like (an unfound object resolves to 0,0), so the
// caller can fall back rather than select in the wrong place.
func (t *NativeTerminalWidget) gridCellAtAbs(abs fyne.Position) (row, col int, ok bool) {
	if t.textGrid == nil {
		return 0, 0, false
	}
	app := fyne.CurrentApp()
	if app == nil {
		return 0, 0, false
	}
	drv := app.Driver()
	if drv == nil || drv.CanvasForObject(t) == nil {
		return 0, 0, false
	}

	local := abs.Subtract(drv.AbsolutePositionForObject(t.textGrid))
	cw, ch := t.gridCellSize()
	size := t.textGrid.Size()
	if local.X < -cw || local.Y < -ch ||
		local.X > size.Width+cw || local.Y > size.Height+ch {
		dprintf("gridCellAtAbs: %v maps outside the grid (local=%v size=%v); falling back\n",
			abs, local, size)
		return 0, 0, false
	}

	row, col = t.textGrid.CursorLocationForPosition(local)
	return row, col, true
}

// gridOrigin is the TextGrid's top-left corner in this widget's coordinate
// space, measured through the driver.
func (t *NativeTerminalWidget) gridOrigin() (fyne.Position, bool) {
	if t.textGrid == nil {
		return fyne.Position{}, false
	}
	app := fyne.CurrentApp()
	if app == nil {
		return fyne.Position{}, false
	}
	drv := app.Driver()
	if drv == nil {
		return fyne.Position{}, false
	}
	// A zero absolute position is what an object outside any canvas
	// reports, and it is indistinguishable from a legitimate top-left
	// corner -- so the canvas is what is checked, not the position.
	if drv.CanvasForObject(t) == nil {
		return fyne.Position{}, false
	}
	self := drv.AbsolutePositionForObject(t)
	grid := drv.AbsolutePositionForObject(t.textGrid)
	return grid.Subtract(self), true
}

func (t *NativeTerminalWidget) DoubleTapped(event *fyne.PointEvent) {
	dprintf("DoubleTapped at %.1f,%.1f\n", event.Position.X, event.Position.Y)

	row, col, ok := t.gridCellAtAbs(event.AbsolutePosition)
	if !ok {
		cw, ch := t.gridCellSize()
		col = 0
		if cw > 0 {
			col = int(event.Position.X / cw)
		}
		row = 0
		if ch > 0 {
			row = int(event.Position.Y / ch)
		}
	}

	// Get display lines
	allLines := t.screen.GetDisplay()
	viewport := t.calculateUnifiedViewport(allLines)

	// Index into the current display window...
	windowRow := viewport.scrollOffset + row
	// ...and the corresponding STABLE absolute virtual-buffer line.
	topAbs := t.screen.GetViewportStart() + viewport.scrollOffset
	absRow := topAbs + row

	if windowRow >= 0 && windowRow < len(allLines) {
		line := allLines[windowRow]
		start, end := t.findWordBoundaries(line, col)

		dprintf("Word boundaries: col %d -> start=%d, end=%d\n", col, start, end)

		// Anchor the selection in absolute coordinates (endCol exclusive).
		t.selection.SetSelection(absRow, start, absRow, end)

		// Get and copy text
		selectedText := t.selection.GetSelectedText()
		dprintf("Selected text: %q\n", selectedText)

		if selectedText != "" {
			t.selection.CopyToClipboard()
		}

		// Force redraw to show selection
		t.updatePending.Store(true)
	}
}

// Add to terminal_events.go

func (t *NativeTerminalWidget) TripleTapped(event *fyne.PointEvent) {
	dprintf("TripleTapped at %.1f,%.1f - selecting entire line\n", event.Position.X, event.Position.Y)

	// Same hit test as MouseDown and DoubleTapped. This used to divide
	// Position.Y by the cell height directly, ignoring the grid's origin
	// entirely -- a third pixel->row mapping for the same question, which is
	// two too many. The arithmetic remains only as the no-canvas fallback.
	row, _, ok := t.gridCellAtAbs(event.AbsolutePosition)
	if !ok {
		_, ch := t.gridCellSize()
		row = 0
		if ch > 0 {
			row = int(event.Position.Y / ch)
		}
	}

	// Get display lines
	allLines := t.screen.GetDisplay()
	viewport := t.calculateUnifiedViewport(allLines)

	// Index into the current display window + the stable absolute line.
	windowRow := viewport.scrollOffset + row
	topAbs := t.screen.GetViewportStart() + viewport.scrollOffset
	absRow := topAbs + row

	if windowRow >= 0 && windowRow < len(allLines) {
		line := allLines[windowRow]

		// Select the entire line (endCol exclusive = rune count).
		t.selection.SetSelection(absRow, 0, absRow, len([]rune(line)))

		// Get and copy text
		selectedText := t.selection.GetSelectedText()
		dprintf("Selected line: %q\n", selectedText)

		if selectedText != "" {
			t.selection.CopyToClipboard()
		}

		// Force redraw to show selection
		t.updatePending.Store(true)
	}
}

// ============================================================================
// desktop.Hoverable interface implementation
// ============================================================================

// MouseIn implements desktop.Hoverable
func (t *NativeTerminalWidget) MouseIn(event *desktop.MouseEvent) {
	// Optional: handle mouse enter - could show cursor or change state
}

// MouseOut implements desktop.Hoverable
func (t *NativeTerminalWidget) MouseOut() {
	// Optional: handle mouse leave
}

// MouseMoved implements desktop.Hoverable
func (t *NativeTerminalWidget) MouseMoved(event *desktop.MouseEvent) {

	// Optional: handle mouse movement for hover effects
}
