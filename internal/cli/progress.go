package cli

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/albertsko/sbxr/internal/image"
)

// progressInterval limits how often the progress line is redrawn.
const progressInterval = 100 * time.Millisecond

// pullProgress returns a callback that draws one progress line for a pull on w and clears it when the pull is done.
// It overwrites the line with spaces instead of escape codes, so that NO_COLOR output stays free of them.
func pullProgress(w io.Writer, ref string) func(image.Progress) {
	var (
		last  time.Time
		width int
	)

	draw := func(line string) {
		pad := max(width-len(line), 0)
		width = len(line)

		_, _ = io.WriteString(w, "\r"+line+strings.Repeat(" ", pad)) // A failed write only skips one update.
	}

	return func(p image.Progress) {
		if p.Done {
			draw("")

			_, _ = io.WriteString(w, "\r") // See above.

			return
		}

		if time.Since(last) < progressInterval {
			return
		}

		last = time.Now()

		draw(fmt.Sprintf("Pulling %s: layer %d/%d, %.1f MB", ref, p.Layer, p.Layers, float64(p.Bytes)/1e6))
	}
}
