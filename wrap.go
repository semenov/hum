package main

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"unicode/utf8"
	"unsafe"
)

// termWidth asks the terminal how wide it is, falling back to something sane
// for pipes and terminals that will not say.
func termWidth() int {
	var ws struct{ rows, cols, x, y uint16 }
	_, _, err := syscall.Syscall(syscall.SYS_IOCTL, os.Stdout.Fd(),
		uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&ws)))
	if err != 0 || ws.cols == 0 {
		return 80
	}
	return int(ws.cols)
}

// wrapper word-wraps a stream that arrives in fragments, keeping continuation
// lines indented under the first one. Tokens do not respect word boundaries, so
// text is held back until a separator makes a word complete.
type wrapper struct {
	width   int // last usable column
	indent  int
	col     int
	pend    strings.Builder
	pendSep bool // a space is owed before the next word
}

func newWrapper(indent int) *wrapper {
	w := termWidth() - 2
	// Long measures are tiring to read; cap regardless of how wide the window is.
	if w > 96 {
		w = 96
	}
	if w < 40 {
		w = 40
	}
	return &wrapper{width: w, indent: indent, col: indent}
}

func (x *wrapper) Write(s string) {
	x.pend.WriteString(s)
	buf := x.pend.String()
	x.pend.Reset()
	for {
		i := strings.IndexAny(buf, " \n")
		if i < 0 {
			x.pend.WriteString(buf)
			return
		}
		x.word(buf[:i])
		if buf[i] == '\n' {
			x.newline()
		} else {
			x.pendSep = true
		}
		buf = buf[i+1:]
	}
}

func (x *wrapper) word(w string) {
	if w == "" {
		return
	}
	n := utf8.RuneCountInString(w)
	if x.pendSep && x.col > x.indent {
		if x.col+1+n > x.width {
			x.newline()
		} else {
			fmt.Print(" ")
			x.col++
		}
	}
	x.pendSep = false
	if x.col+n > x.width && x.col > x.indent {
		x.newline()
	}
	fmt.Print(w)
	x.col += n
}

func (x *wrapper) newline() {
	fmt.Print("\n" + strings.Repeat(" ", x.indent))
	x.col = x.indent
	x.pendSep = false
}

// Flush emits whatever is still held back at the end of a reply.
func (x *wrapper) Flush() {
	if s := x.pend.String(); s != "" {
		x.pend.Reset()
		x.word(s)
	}
}
