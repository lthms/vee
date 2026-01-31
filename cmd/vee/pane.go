package main

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/creack/pty"
	"github.com/hinshun/vt10x"
)

// Pane represents a multiplexed session with its own PTY and VTE.
type Pane struct {
	ID        string
	Mode      Mode
	SessionID string
	Preview   string

	ptmx    *os.File       // master side of the PTY
	process *exec.Cmd      // the claude process
	vt      vt10x.Terminal // virtual terminal tracking screen state
	doneCh  chan struct{}   // closed when process exits
	exitErr error          // exit error, set before doneCh is closed

	mu     sync.Mutex
	closed bool
}

// newPane allocates a PTY, spawns the claude process, and starts a VTE reader goroutine.
// The outputFn callback is called whenever new output is available (for bubbletea message dispatch).
func newPane(id string, mode Mode, sessionID string, preview string, args []string, cols, rows int, outputFn func()) (*Pane, error) {
	// Subtract 1 row for the status bar
	paneRows := rows - 1
	if paneRows < 1 {
		paneRows = 1
	}

	vt := vt10x.New(vt10x.WithSize(cols, paneRows))

	cmd := exec.Command("claude", args...)
	// Build environment, ensuring TERM supports colors
	env := os.Environ()
	hasTerm := false
	for _, e := range env {
		if len(e) > 5 && e[:5] == "TERM=" {
			hasTerm = true
			break
		}
	}
	if !hasTerm {
		env = append(env, "TERM=xterm-256color")
	}
	cmd.Env = append(env,
		fmt.Sprintf("COLUMNS=%d", cols),
		fmt.Sprintf("LINES=%d", paneRows),
	)

	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{
		Cols: uint16(cols),
		Rows: uint16(paneRows),
	})
	if err != nil {
		return nil, fmt.Errorf("pty start: %w", err)
	}

	p := &Pane{
		ID:        id,
		Mode:      mode,
		SessionID: sessionID,
		Preview:   preview,
		ptmx:      ptmx,
		process:   cmd,
		vt:        vt,
		doneCh:    make(chan struct{}),
	}

	// Wait for process exit in background
	go func() {
		p.exitErr = cmd.Wait()
		close(p.doneCh)
	}()

	// Read PTY output and feed to VTE
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				p.vt.Write(buf[:n])
				if outputFn != nil {
					outputFn()
				}
			}
			if err != nil {
				return
			}
		}
	}()

	return p, nil
}

// writeInput sends raw bytes to the PTY (from keyboard).
func (p *Pane) writeInput(data []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	_, _ = p.ptmx.Write(data)
}

// resize updates the PTY and VTE dimensions.
func (p *Pane) resize(cols, rows int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}

	// Subtract 1 row for the status bar
	paneRows := rows - 1
	if paneRows < 1 {
		paneRows = 1
	}

	p.vt.Resize(cols, paneRows)

	// Set PTY size
	ws := struct {
		Row    uint16
		Col    uint16
		Xpixel uint16
		Ypixel uint16
	}{
		Row: uint16(paneRows),
		Col: uint16(cols),
	}
	_, _, _ = syscall.Syscall(
		syscall.SYS_IOCTL,
		p.ptmx.Fd(),
		syscall.TIOCSWINSZ,
		uintptr(unsafe.Pointer(&ws)),
	)
}

// render returns the current VTE screen content with ANSI color/attribute codes.
func (p *Pane) render() string {
	p.vt.Lock()
	defer p.vt.Unlock()

	cols, rows := p.vt.Size()
	var sb strings.Builder
	sb.Grow(cols * rows * 2) // rough estimate

	const (
		attrReverse   = 1 << 0
		attrUnderline = 1 << 1
		attrBold      = 1 << 2
		// attrGfx    = 1 << 3 (not visual)
		attrItalic = 1 << 4
		attrBlink  = 1 << 5
	)

	// Track current style to avoid redundant escape sequences
	var curFG, curBG vt10x.Color
	var curMode int16
	curFG = vt10x.DefaultFG
	curBG = vt10x.DefaultBG
	curMode = 0
	firstCell := true

	for y := 0; y < rows; y++ {
		if y > 0 {
			sb.WriteByte('\n')
		}
		// Reset at start of each line to avoid style bleed
		sb.WriteString("\x1b[0m")
		curFG = vt10x.DefaultFG
		curBG = vt10x.DefaultBG
		curMode = 0
		firstCell = true

		for x := 0; x < cols; x++ {
			cell := p.vt.Cell(x, y)

			// Undo vt10x's internal color transforms.
			// vt10x swaps FG↔BG for reverse cells in setChar(), but keeps
			// attrReverse in Mode. We undo the swap so we can emit SGR 7
			// and let the terminal handle reversal (correctly handles default colors).
			if cell.Mode&int16(attrReverse) != 0 {
				cell.FG, cell.BG = cell.BG, cell.FG
			}
			// vt10x promotes standard colors (0-7) to bright (8-15) for bold
			// cells in setChar(). Undo this so the terminal applies bold→bright
			// naturally, producing colors that match the user's terminal theme.
			if cell.Mode&int16(attrBold) != 0 && cell.FG >= 8 && cell.FG < 16 {
				cell.FG -= 8
			}

			// Emit style changes if attributes differ from current
			needReset := false
			if !firstCell {
				// Check if we need a full reset (removed bold, underline, etc.)
				oldBold := curMode&int16(attrBold) != 0
				newBold := cell.Mode&int16(attrBold) != 0
				oldUnderline := curMode&int16(attrUnderline) != 0
				newUnderline := cell.Mode&int16(attrUnderline) != 0
				oldItalic := curMode&int16(attrItalic) != 0
				newItalic := cell.Mode&int16(attrItalic) != 0
				oldReverse := curMode&int16(attrReverse) != 0
				newReverse := cell.Mode&int16(attrReverse) != 0
				oldBlink := curMode&int16(attrBlink) != 0
				newBlink := cell.Mode&int16(attrBlink) != 0

				if (oldBold && !newBold) || (oldUnderline && !newUnderline) ||
					(oldItalic && !newItalic) || (oldReverse && !newReverse) ||
					(oldBlink && !newBlink) {
					needReset = true
				}
			}

			if firstCell || needReset || cell.FG != curFG || cell.BG != curBG || cell.Mode != curMode {
				var codes []string
				if firstCell || needReset {
					codes = append(codes, "0") // SGR reset
				}
				if cell.Mode&int16(attrBold) != 0 {
					codes = append(codes, "1")
				}
				if cell.Mode&int16(attrItalic) != 0 {
					codes = append(codes, "3")
				}
				if cell.Mode&int16(attrUnderline) != 0 {
					codes = append(codes, "4")
				}
				if cell.Mode&int16(attrBlink) != 0 {
					codes = append(codes, "5")
				}
				if cell.Mode&int16(attrReverse) != 0 {
					codes = append(codes, "7")
				}
				if cell.FG != vt10x.DefaultFG {
					if s := colorToANSI(cell.FG, true); s != "" {
						codes = append(codes, s)
					}
				}
				if cell.BG != vt10x.DefaultBG {
					if s := colorToANSI(cell.BG, false); s != "" {
						codes = append(codes, s)
					}
				}
				if len(codes) > 0 {
					sb.WriteString("\x1b[")
					sb.WriteString(strings.Join(codes, ";"))
					sb.WriteByte('m')
				}

				curFG = cell.FG
				curBG = cell.BG
				curMode = cell.Mode
				firstCell = false
			}

			ch := cell.Char
			if ch == 0 {
				ch = ' '
			}
			sb.WriteRune(ch)
		}
	}

	// Reset at the end
	sb.WriteString("\x1b[0m")

	return sb.String()
}

// colorToANSI converts a vt10x.Color to an ANSI SGR parameter string.
func colorToANSI(c vt10x.Color, fg bool) string {
	if c >= vt10x.DefaultFG {
		// Default color — no code needed (handled by SGR reset)
		return ""
	}
	if c < 8 {
		// Standard ANSI colors
		if fg {
			return fmt.Sprintf("%d", 30+int(c))
		}
		return fmt.Sprintf("%d", 40+int(c))
	}
	if c < 16 {
		// Bright ANSI colors
		if fg {
			return fmt.Sprintf("%d", 90+int(c)-8)
		}
		return fmt.Sprintf("%d", 100+int(c)-8)
	}
	if c < 256 {
		// 256-color palette
		if fg {
			return fmt.Sprintf("38;5;%d", int(c))
		}
		return fmt.Sprintf("48;5;%d", int(c))
	}
	// RGB color (encoded as r<<16 | g<<8 | b)
	r := (int(c) >> 16) & 0xFF
	g := (int(c) >> 8) & 0xFF
	b := int(c) & 0xFF
	if fg {
		return fmt.Sprintf("38;2;%d;%d;%d", r, g, b)
	}
	return fmt.Sprintf("48;2;%d;%d;%d", r, g, b)
}

// close sends SIGINT and waits for graceful exit, then closes the PTY.
func (p *Pane) close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.mu.Unlock()

	if p.process.Process != nil {
		_ = p.process.Process.Signal(syscall.SIGINT)
	}

	// Wait up to 5 seconds for graceful exit
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-p.doneCh:
	case <-timer.C:
		slog.Debug("force killing pane process", "pane", p.ID)
		if p.process.Process != nil {
			_ = p.process.Process.Kill()
		}
		<-p.doneCh
	}

	_ = p.ptmx.Close()
}

// done returns a channel that is closed when the process exits.
// Safe to read from multiple goroutines.
func (p *Pane) done() <-chan struct{} {
	return p.doneCh
}

// isAlive returns true if the pane's process hasn't exited yet.
func (p *Pane) isAlive() bool {
	select {
	case <-p.doneCh:
		return false
	default:
		return true
	}
}
