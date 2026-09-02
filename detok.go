// Streaming detokenisation. The worker exports an id -> utf-8 bytes table at
// startup, so this is a table lookup and a rune boundary check rather than a
// call back into Python for every token.
package main

import (
	"unicode/utf8"
)

// ---- streaming detokenizer (byte-level BPE) --------------------------------

type Detok struct {
	vocab [][]byte
	buf   []byte // bytes not yet emitted (incomplete utf-8)
}

// Add returns the newly decodable text for this token.
func (d *Detok) Add(id uint32) string {
	if int(id) < len(d.vocab) {
		d.buf = append(d.buf, d.vocab[id]...)
	}
	// emit only complete utf-8 runes
	good := len(d.buf)
	for good > 0 {
		r, sz := utf8.DecodeLastRune(d.buf[:good])
		if r == utf8.RuneError && sz <= 1 {
			good-- // trailing partial rune, hold it back
			if len(d.buf)-good > 4 {
				good = len(d.buf) // not a partial rune, it's genuinely invalid
				break
			}
			continue
		}
		break
	}
	if good == 0 {
		return ""
	}
	s := string(d.buf[:good])
	d.buf = append(d.buf[:0], d.buf[good:]...)
	return s
}
