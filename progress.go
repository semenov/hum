package main

import (
	"fmt"
	"math"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Shown one at a time while the weights come down, so the wait has something
// to read. Kept short enough to fit a terminal line.
var quotes = []string{
	"Teaching sand to think was, in hindsight, a bold move.",
	"Somewhere in these weights is a number that means \"cat\". Nobody knows which one.",
	"Every parameter is a small opinion about the world.",
	"It has read more than you have, and understood less. Possibly the other way round.",
	"No one has ever inspected layer 47. It is doing fine.",
	"Attention is all you need — inconveniently, attention is also the expensive part.",
	"4-bit weights: each number gets sixteen possible beliefs.",
	"The fans are about to have opinions.",
	"Gradient descent is just falling downhill, very carefully, for a month.",
	"It dreams only when asked, and forgets before you finish reading.",
	"Three billion parameters awake at a time. The rest are resting.",
	"None of these numbers know what they mean. Together they know almost everything.",
	"The tokenizer has strong feelings about whitespace. Do not ask it about them.",
	"Loading a mind that has never had a body.",
	"Compression is understanding. Nobody is entirely sure why.",
	"Every answer is a confident guess. Some of them are also correct.",
	"It memorised the shape of language, then inferred the rest.",
	"At temperature zero it is at its most certain and its most boring.",
	"This model has never seen a photograph and holds firm views on colour.",
	"Your Mac is about to get warm. That warmth is the thinking.",
}

// Progress renders a download bar with a rotating quote underneath. On a
// non-terminal it degrades to occasional plain lines, so logs stay readable.
type Progress struct {
	total   int64
	done    atomic.Int64
	start   time.Time
	tty     bool
	p       palette
	stop    chan struct{}
	wg      sync.WaitGroup
	lastLog time.Time
	quote   int

	// The right-hand figures are refreshed on their own slow cadence. Recomputing
	// them every frame makes the line twitch without telling you anything more.
	rateAt    time.Time
	rateBytes int64
	smoothed  float64 // bytes/s, exponentially weighted
	rateText  string
	etaText   string
}

func NewProgress(total int64) *Progress {
	pal := newPalette()
	return &Progress{
		total: total,
		start: time.Now(),
		tty:   pal.on,
		p:     pal,
		stop:  make(chan struct{}),
	}
}

// Write makes Progress an io.Writer, so it can be dropped into an io.MultiWriter.
func (p *Progress) Write(b []byte) (int, error) {
	p.done.Add(int64(len(b)))
	return len(b), nil
}

func (p *Progress) Add(n int64) { p.done.Add(n) }

func (p *Progress) Start() {
	if p.tty {
		fmt.Printf("\n  %s\n\n", p.p.green("Download progress"))
		fmt.Print("\033[?25l") // hide the cursor: it parks on the bar and reads as a block
		// Restore the cursor if the download is interrupted, so Ctrl-C does not
		// leave the terminal without one.
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		go func() {
			<-sig
			fmt.Print("\033[?25h\n")
			os.Exit(130)
		}()
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		tick := time.NewTicker(120 * time.Millisecond)
		defer tick.Stop()
		rotate := time.NewTicker(7 * time.Second)
		defer rotate.Stop()
		p.render()
		for {
			select {
			case <-p.stop:
				return
			case <-rotate.C:
				p.quote = (p.quote + 1) % len(quotes)
			case <-tick.C:
				p.render()
			}
		}
	}()
}

func (p *Progress) Stop() {
	close(p.stop)
	p.wg.Wait()
	if p.tty {
		// Clear all four lines of the block, come back to the top so the summary
		// prints where the bar was, and give the cursor back.
		fmt.Print("\r\033[K\n\r\033[K\n\r\033[K\n\r\033[K\033[3A\r\033[?25h")
	}
	d := time.Since(p.start).Round(time.Second)
	fmt.Printf("\n  Downloaded %s in %s. It is now cached and will not be fetched again.\n",
		humanBytes(p.done.Load()), d)
}

func (p *Progress) render() {
	done, total := p.done.Load(), p.total
	if total <= 0 {
		total = 1
	}
	frac := float64(done) / float64(total)
	if frac > 1 {
		frac = 1
	}
	elapsed := time.Since(p.start).Seconds()
	speed := float64(done) / elapsed
	if p.rateText == "" || time.Since(p.rateAt) >= 2*time.Second {
		// Rate over the last window, smoothed. A cumulative average drags the
		// slow first seconds along with it, which is what made the estimate
		// swing from an hour to seven minutes while nothing had changed.
		now := time.Now()
		if dt := now.Sub(p.rateAt).Seconds(); dt > 0 && !p.rateAt.IsZero() {
			inst := float64(done-p.rateBytes) / dt
			if p.smoothed == 0 {
				p.smoothed = inst
			} else {
				p.smoothed = 0.7*p.smoothed + 0.3*inst
			}
		} else {
			p.smoothed = speed
		}
		p.rateAt, p.rateBytes = now, done
		p.rateText = humanBytes(int64(p.smoothed)) + "/s"
		p.etaText = "--"
		if p.smoothed > 0 && done < total {
			p.etaText = etaMinutes(float64(total-done) / p.smoothed)
		}
	}

	if !p.tty {
		// Plain mode: one line every 5s, no escape codes.
		if time.Since(p.lastLog) < 5*time.Second {
			return
		}
		p.lastLog = time.Now()
		fmt.Printf("  Downloading… %.2f%%  %s of %s  at %s/s\n", frac*100,
			humanBytes(done), humanBytes(total), humanBytes(int64(speed)))
		return
	}

	const w = 34
	full := int(frac * float64(w))
	// The filled cells carry the same blue-to-green fade as the wordmark, keyed
	// to position in the whole bar so the colours do not shift as it fills.
	var bar strings.Builder
	for i := 0; i < w; i++ {
		if i < full {
			t := float64(i) / float64(w-1)
			bar.WriteString(p.p.rgb(
				int(78+(61-78)*t), int(168+(220-168)*t), int(255+(132-255)*t), "█"))
		} else {
			bar.WriteString(p.p.dim("░"))
		}
	}
	line := fmt.Sprintf("  %s  %s  %s  %s  %s",
		bar.String(),
		p.p.bold(fmt.Sprintf("%6.2f%%", frac*100)),
		p.p.dim(fmt.Sprintf("%s / %s", humanBytes(done), humanBytes(total))),
		p.p.dim(p.rateText),
		p.p.dim("eta "+p.etaText))
	q := "  " + p.p.rgb(122, 162, 200, quotes[p.quote])

	// Four lines: bar, blank, quote, blank. Redraw them all each tick and return
	// the cursor to the top of the block.
	fmt.Printf("\r\033[K%s\n\r\033[K\n\r\033[K%s\n\r\033[K\033[3A\r", line, q)
}

// etaMinutes reports whole minutes. Seconds on a twenty-minute download are
// noise: they change constantly and nobody acts on them.
func etaMinutes(seconds float64) string {
	m := int(math.Ceil(seconds / 60))
	switch {
	case m >= 60:
		return fmt.Sprintf("%dh %dm", m/60, m%60)
	case m >= 1:
		return fmt.Sprintf("%dm", m)
	default:
		return "<1m"
	}
}

func humanBytes(n int64) string {
	switch {
	case n >= gb:
		return fmt.Sprintf("%.1f GB", float64(n)/gb)
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
