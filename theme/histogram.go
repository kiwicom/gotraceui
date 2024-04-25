package theme

import (
	"context"
	"fmt"
	"image"
	"math"
	rtrace "runtime/trace"
	"time"

	"gioui.org/io/event"
	"honnef.co/go/gotraceui/clip"
	"honnef.co/go/gotraceui/color"
	"honnef.co/go/gotraceui/gesture"
	"honnef.co/go/gotraceui/layout"
	"honnef.co/go/gotraceui/widget"

	"gioui.org/f32"
	"gioui.org/font"
	"gioui.org/io/key"
	"gioui.org/io/pointer"
	"gioui.org/op"
	"gioui.org/text"
	"gioui.org/unit"
)

type HistogramState struct {
	Histogram *widget.Histogram

	hover        gesture.Hover
	click        gesture.Click
	prevBarWidth float32

	dragging struct {
		active      bool
		startBucket int
	}
}

type HistogramStyle struct {
	State *HistogramState

	XLabel, YLabel   string
	TextColor        color.Oklch
	TextSize         unit.Sp
	LineColor        color.Oklch
	BinColor         color.Oklch
	HoveredBinColor  color.Oklch
	SelectedBinColor color.Oklch
	OverflowBinColor color.Oklch
}

func Histogram(th *Theme, state *HistogramState) HistogramStyle {
	return HistogramStyle{
		State:            state,
		TextColor:        th.Palette.Foreground,
		TextSize:         th.TextSize,
		LineColor:        th.Palette.Border,
		BinColor:         oklch(54.01, 0.139, 248.98),
		HoveredBinColor:  oklch(69.06, 0.224, 141.9),
		SelectedBinColor: oklch(69.06, 0.224, 141.9),
		OverflowBinColor: oklch(50.62, 0.195, 27.95),
	}
}

func (hs *HistogramState) Update(gtx layout.Context) (start, end widget.FloatDuration, ok bool) {
	if hs.Histogram == nil {
		return 0, 0, false
	}

	clicked := false
	for _, click := range hs.click.Update(gtx.Queue) {
		if click.Button != pointer.ButtonPrimary {
			continue
		}
		if click.Kind != gesture.KindClick {
			continue
		}
		if click.NumClicks == 2 {
			clicked = true
		}
	}

	hovered := hs.hover.Update(gtx)

	var (
		trackDragStart bool
		trackDragEnd   bool
	)

	// pointer.InputOp{Tag: hs.State, Kinds: pointer.Press | pointer.Release | pointer.Drag | pointer.Cancel}.Add(gtx.Ops)
	//for _, ev := range gtx.Events(hs) {
	for {
		ev, ok := gtx.Event(
			pointer.Filter{
				Target: hs.State,
				Kinds:  pointer.Press | pointer.Release | pointer.Drag | pointer.Cancel,
			},
		)
		if !ok {
			break
		}
		if ev, ok := ev.(pointer.Event); ok {
			switch ev.Kind {
			case pointer.Press:
				if ev.Modifiers == key.ModShortcut {
					hs.dragging.active = true
					trackDragStart = true
				}
			case pointer.Release:
				if hs.dragging.active {
					trackDragEnd = true
				}
				hs.dragging.active = false
			case pointer.Cancel:
				hs.dragging.active = false
			}
		}
	}

	// In theory, it should be impossible for hovered to be false while any click happened, but let's be on
	// the safe side.
	if hovered {
		bin := int(hs.hover.Pointer().X / hs.prevBarWidth)
		if bin < 0 {
			bin = 0
		} else if bin >= len(hs.Histogram.Bins) {
			bin = len(hs.Histogram.Bins) - 1
		}

		if clicked {
			start, end = hs.Histogram.BucketRange(bin)
			if bin == len(hs.Histogram.Bins) {
				// The final bin is closed
				end += 1
			}
		}

		if trackDragStart {
			hs.dragging.startBucket = bin
		}

		if trackDragEnd {
			start, end = hs.selectedRange(bin)
			hs.dragging.active = false
			hs.dragging.startBucket = 0
		}
	}

	return start, end, !(start == 0 && end == 0)
}

func (hs *HistogramState) selectedRange(bin int) (start, end widget.FloatDuration) {
	if bin > hs.dragging.startBucket {
		start, _ = hs.Histogram.BucketRange(hs.dragging.startBucket)
		_, end = hs.Histogram.BucketRange(bin)
	} else {
		_, end = hs.Histogram.BucketRange(hs.dragging.startBucket)
		start, _ = hs.Histogram.BucketRange(bin)
	}
	return start, end
}

func (hs HistogramStyle) Layout(win *Window, gtx layout.Context) layout.Dimensions {
	defer rtrace.StartRegion(context.Background(), "theme.HistogramStyle.Layout").End()

	hist := hs.State.Histogram

	roundf := func(f float32) float32 {
		return float32(math.Round(float64(f)))
	}

	binXCoordinates := func(bin int, barWidth float32) (int, int) {
		x0 := int(roundf(float32(bin) * barWidth))
		x1 := int(roundf(float32(bin+1) * barWidth))
		return x0, x1
	}

	defer clip.Rect{Max: gtx.Constraints.Min}.Push(gtx.Ops).Pop()

	gtx.Constraints.Max = gtx.Constraints.Min

	hovered := hs.State.hover.Update(gtx.Queue)

	var (
		tickLength    = gtx.Dp(10)
		tickThickness = gtx.Dp(1)
		padding       = gtx.Dp(2)
		borderWidth   = gtx.Dp(1)
	)

	var (
		xAxisHeight int
		yAxisWidth  int
		lineHeight  int
	)

	// Compute Y axis width
	{
		m := op.Record(gtx.Ops)
		gtx := gtx
		gtx.Constraints.Min = image.Point{}
		gtx.Constraints.Max = image.Point{9999, 9999}
		dims := widget.Label{}.Layout(gtx, win.Theme.Shaper, font.Font{}, hs.TextSize, "9.99E+99", win.ColorMaterial(gtx, hs.TextColor))
		m.Stop()
		lineHeight = dims.Size.Y

		// Y axis width = tick number width + tick length. The label is placed between the ticks
		yAxisWidth = dims.Size.X + tickLength
	}

	// Compute X axis height
	// X axis height = label line height + tick number height + tick length + various padding
	xAxisHeight = lineHeight + padding + lineHeight + tickLength

	plotWidth := gtx.Constraints.Min.X - yAxisWidth
	plotHeight := gtx.Constraints.Min.Y - xAxisHeight
	barWidth := float32(plotWidth-borderWidth) / float32(len(hist.Bins))

	// Draw Y Axis
	func() {
		defer clip.Rect{Min: image.Pt(0, 0), Max: image.Pt(yAxisWidth, plotHeight)}.Push(gtx.Ops).Pop()
		gtx := gtx

		// Draw top Y tick
		FillShape(win, gtx.Ops, hs.LineColor, clip.Rect{Min: image.Pt(yAxisWidth-tickLength, 0), Max: image.Pt(yAxisWidth, tickThickness)}.Op())

		// Draw bottom Y tick
		FillShape(win, gtx.Ops, hs.LineColor, clip.Rect{Min: image.Pt(yAxisWidth-tickLength, plotHeight-tickThickness), Max: image.Pt(yAxisWidth, plotHeight)}.Op())

		func() {
			// Draw top Y tick label
			gtx := gtx
			gtx.Constraints.Min.X = yAxisWidth - tickLength
			widget.Label{Alignment: text.End}.Layout(gtx, win.Theme.Shaper, font.Font{}, hs.TextSize, fmt.Sprintf("%.2e", float64(hist.MaxBinValue)), win.ColorMaterial(gtx, hs.TextColor))

			// Draw bottom Y tick label
			defer op.Offset(image.Pt(0, plotHeight-lineHeight)).Push(gtx.Ops).Pop()
			widget.Label{Alignment: text.End}.Layout(gtx, win.Theme.Shaper, font.Font{}, hs.TextSize, "0", win.ColorMaterial(gtx, hs.TextColor))
		}()

		// Draw Y label
		m := op.Record(gtx.Ops)
		gtx.Constraints.Min = image.Point{}
		gtx.Constraints.Max = image.Point{9999, 9999}
		dims := widget.Label{MaxLines: 1}.Layout(gtx, win.Theme.Shaper, font.Font{}, hs.TextSize, hs.YLabel, win.ColorMaterial(gtx, hs.TextColor))
		c := m.Stop()

		aff := f32.Affine2D{}.
			Rotate(f32.Point{}, -1.57).
			Offset(f32.Pt(float32(yAxisWidth-tickLength-lineHeight), float32(plotHeight)/2+float32(dims.Size.X)/2))
		defer op.Affine(aff).Push(gtx.Ops).Pop()
		c.Add(gtx.Ops)
	}()

	// Draw X axis
	func() {
		defer op.Offset(image.Pt(yAxisWidth, plotHeight)).Push(gtx.Ops).Pop()
		defer clip.Rect{Max: image.Pt(plotWidth, xAxisHeight)}.Push(gtx.Ops).Pop()
		gtx := gtx
		gtx.Constraints.Min = image.Point{}
		gtx.Constraints.Max = image.Point{9999, 9999}

		// Draw first X tick
		FillShape(win, gtx.Ops, hs.LineColor, clip.Rect{Max: image.Pt(tickThickness, tickLength)}.Op())

		// Draw last X tick
		var lastTickX int
		if hist.HasOverflow() {
			lastTickX = int(roundf(float32(len(hist.Bins)-1) * barWidth))
		} else {
			lastTickX = int(roundf(float32(len(hist.Bins)) * barWidth))
		}
		FillShape(win, gtx.Ops, hs.LineColor, clip.Rect{Min: image.Pt(lastTickX, 0), Max: image.Pt(lastTickX+2, 20)}.Op())

		// Draw X tick labels and range
		//
		// Move below the X ticks
		defer op.Offset(image.Pt(0, 20)).Push(gtx.Ops).Pop()
		// Clip X to the last X tick, which might be before the end of the histogram. We do so to
		// center the axis label.
		defer clip.Rect{Max: image.Pt(lastTickX, gtx.Constraints.Max.Y)}.Push(gtx.Ops).Pop()
		gtx.Constraints.Min.X = lastTickX
		gtx.Constraints.Max.X = lastTickX

		var end widget.FloatDuration
		var numBins int
		if hist.HasOverflow() {
			_, end = hist.BucketRange(len(hist.Bins) - 2)
			numBins = len(hist.Bins) - 1
		} else {
			_, end = hist.BucketRange(len(hist.Bins) - 1)
			numBins = len(hist.Bins)
		}

		availableWidth := lastTickX
		var firstXTickLabelWidth int

		// Layout and measure first X axis tick
		{
			gtx := gtx
			gtx.Constraints.Min.X = 0
			dims := widget.Label{Alignment: text.Start}.Layout(gtx, win.Theme.Shaper, font.Font{}, hs.TextSize, hist.Start.Ceil().String(), win.ColorMaterial(gtx, hs.TextColor))
			availableWidth -= dims.Size.X
			firstXTickLabelWidth = dims.Size.X
		}

		// Measure last X axis tick
		{
			gtx := gtx
			m := op.Record(gtx.Ops)
			gtx.Constraints.Min.X = 0
			dims := widget.Label{Alignment: text.Start}.Layout(gtx, win.Theme.Shaper, font.Font{}, hs.TextSize, end.Ceil().String(), win.ColorMaterial(gtx, hs.TextColor))
			m.Stop()
			availableWidth -= dims.Size.X

		}

		// Layout last X axis tick
		widget.Label{Alignment: text.End}.Layout(gtx, win.Theme.Shaper, font.Font{}, hs.TextSize, end.Ceil().String(), win.ColorMaterial(gtx, hs.TextColor))

		// Measure X axis info
		var line string
		{
			gtx := gtx
			gtx.Constraints.Min.X = 0

			m := op.Record(gtx.Ops)
			line = fmt.Sprintf("⬅ %d×~%s = %s ➡", numBins, hist.BinWidth.Floor(), (end - hist.Start).Ceil())
			dims := widget.Label{Alignment: text.Start}.Layout(gtx, win.Theme.Shaper, font.Font{}, hs.TextSize, line, win.ColorMaterial(gtx, hs.TextColor))
			m.Stop()
			if dims.Size.X > availableWidth {
				line = fmt.Sprintf("⬅ %s ➡", (end - hist.Start).Ceil())

				m := op.Record(gtx.Ops)
				dims := widget.Label{Alignment: text.Start}.Layout(gtx, win.Theme.Shaper, font.Font{}, hs.TextSize, line, win.ColorMaterial(gtx, hs.TextColor))
				m.Stop()
				if dims.Size.X > availableWidth {
					line = ""
				}
			}
		}
		if line != "" {
			gtx := gtx
			stack := op.Offset(image.Pt(firstXTickLabelWidth, 0)).Push(gtx.Ops)
			gtx.Constraints.Min.X = availableWidth
			gtx.Constraints.Max.X = gtx.Constraints.Min.X
			widget.Label{Alignment: text.Middle}.Layout(gtx, win.Theme.Shaper, font.Font{}, hs.TextSize, line, win.ColorMaterial(gtx, hs.TextColor))
			stack.Pop()
		}

		// Draw X label
		defer op.Offset(image.Pt(0, lineHeight+padding)).Push(gtx.Ops).Pop()
		widget.Label{Alignment: text.Middle}.Layout(gtx, win.Theme.Shaper, font.Font{}, hs.TextSize, hs.XLabel, win.ColorMaterial(gtx, hs.TextColor))
	}()

	// Draw plot
	func() {
		defer op.Offset(image.Pt(yAxisWidth, 0)).Push(gtx.Ops).Pop()
		defer clip.Rect{Max: image.Pt(plotWidth, plotHeight)}.Push(gtx.Ops).Pop()

		// These lines have no transparency and are pixel perfect, so overlap isn't a problem.
		FillShape(win, gtx.Ops, hs.LineColor, clip.Rect{Max: image.Pt(borderWidth, plotHeight)}.Op())
		FillShape(win, gtx.Ops, hs.LineColor, clip.Rect{Min: image.Pt(0, plotHeight-borderWidth), Max: image.Pt(plotWidth, plotHeight)}.Op())

		defer op.Offset(image.Pt(borderWidth, 0)).Push(gtx.Ops).Pop()
		plotWidth := plotWidth - borderWidth
		plotHeight := plotHeight - borderWidth
		// Clip again so that the pointer coordinates are relative to the previously set offset.
		defer clip.Rect{Max: image.Pt(plotWidth, plotHeight)}.Push(gtx.Ops).Pop()

		gtx := gtx
		gtx.Constraints.Min = image.Point{plotWidth, plotHeight}
		gtx.Constraints.Max = gtx.Constraints.Min

		event.Op(gtx.Ops, hs.State)
		hs.State.click.Add(gtx.Ops)
		hs.State.hover.Add(gtx.Ops)

		hBin := int(hs.State.hover.Pointer().X / barWidth)

		if hBin < 0 {
			hBin = 0
		} else if hBin >= len(hist.Bins) {
			hBin = len(hist.Bins) - 1
		}

		// Say we have a floating-point bin range of [140.40ns, 280.80ns) – we don't want to
		// deal with displaying sub-nanosecond precision numbers, so instead we round the
		// numbers to [141ns, 281ns). We can do this because our actual measurements are
		// integers with nanosecond precision. For a bound of x.yy ns, there exists no value
		// lower than x but lower than x+1.
		//
		// We always round up both bounds. [140.40ns, ...) will be preceeded by [..., 140.40ns)
		// - thus 140 fits into the preceeding bin, and the following bin starts at 141ns. The
		// preceeding bin's upper bound is exclusive, so there is no overlap between bins when
		// rounding.
		if hs.State.dragging.active {
			startf, endf := hs.State.selectedRange(hBin)
			start, end := startf.Ceil(), endf.Ceil()
			var (
				s string
				c rune
			)
			if hBin == len(hist.Bins)-1 || hs.State.dragging.startBucket == len(hist.Bins)-1 {
				c = ']'
			} else {
				c = ')'
			}
			s = fmt.Sprintf("Selected range: [%s, %s%c", start, end, c)
			win.SetTooltip(func(win *Window, gtx layout.Context) layout.Dimensions {
				return Tooltip(win.Theme, s).Layout(win, gtx)
			})
		} else if hovered {
			var (
				s            string
				lower, upper time.Duration
				closing      rune
			)
			if !hist.HasOverflow() || hBin != len(hist.Bins)-1 {
				lower = (hist.Start + hist.BinWidth*widget.FloatDuration(hBin)).Ceil()
				upper = (hist.Start + hist.BinWidth*widget.FloatDuration(hBin+1)).Ceil()
			} else {
				lower = time.Duration(math.Ceil(float64(hist.Overflow)))
				upper = hist.MaxValue
			}
			if hBin == len(hist.Bins)-1 {
				closing = ']'
			} else {
				closing = ')'
			}
			s = fmt.Sprintf("Range: [%s, %s%c\nValue: %d", lower, upper, closing, hist.Bins[hBin])
			win.SetTooltip(func(win *Window, gtx layout.Context) layout.Dimensions {
				return Tooltip(win.Theme, s).Layout(win, gtx)
			})

		}

		for i, bin := range hist.Bins {
			x0, x1 := binXCoordinates(i, barWidth)
			y0 := gtx.Constraints.Min.Y
			var y1 int
			if hist.MaxBinValue == 0 {
				// Don't draw bars for zero bins, even if all bins are zero
				y1 = y0
			} else {
				y1 = int(roundf(float32(gtx.Constraints.Min.Y) - float32(gtx.Constraints.Min.Y)*(float32(bin)/float32(hist.MaxBinValue))))
			}

			rect := clip.Rect{
				Min: image.Pt(x0, y1),
				Max: image.Pt(x1, y0),
			}

			var c color.Oklch
			if hovered && i == hBin {
				// Hovered bin
				c = hs.HoveredBinColor
			} else {
				if !hist.HasOverflow() || i != len(hist.Bins)-1 {
					// Normal bin
					c = hs.BinColor
				} else {
					// Overflow bin
					c = hs.OverflowBinColor
				}
			}

			if hs.State.dragging.active {
				if hBin >= hs.State.dragging.startBucket {
					if i >= hs.State.dragging.startBucket && i <= hBin {
						// Selected bin (dragging)
						c = hs.SelectedBinColor
					}
				} else {
					if i <= hs.State.dragging.startBucket && i >= hBin {
						// Selected bin (dragging)
						c = hs.SelectedBinColor
					}
				}
			}

			FillShape(win, gtx.Ops, c, rect.Op())
		}
	}()

	return layout.Dimensions{
		Size: gtx.Constraints.Min,
	}
}
