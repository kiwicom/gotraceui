package main

import (
	"context"
	"image"
	"image/color"
	"math"
	rtrace "runtime/trace"
	"sort"
	"time"

	"gioui.org/io/event"
	myclip "honnef.co/go/gotraceui/clip"
	"honnef.co/go/gotraceui/layout"
	"honnef.co/go/gotraceui/theme"
	"honnef.co/go/gotraceui/trace/ptrace"
	"honnef.co/go/gotraceui/widget"

	"gioui.org/f32"
	"gioui.org/io/key"
	"gioui.org/io/pointer"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
)

type heatmapCacheKey struct {
	size            image.Point
	useLinearColors bool
	yBucketSize     int
	xBucketSize     time.Duration
}

type Heatmap struct {
	MaxY int

	// These values can be changed and the heatmap will update accordingly.
	UseLinearColors bool
	XBucketSize     time.Duration
	YBucketSize     int

	numXBuckets int
	numYBuckets int
	// data represents the absolute value of each bucket, laid out in column-major order.
	data []int

	// We store the original data as this allows us to change the yStep and recompute the buckets.
	origData [][]int

	pointer f32.Point
	// pointerConstraint records the constraint when we captured the pointer position. This is to avoid using outdated
	// positions when the window size changes without causing new pointer move events.
	pointerConstraint image.Point

	hovered HeatmapBucket

	cacheKey    heatmapCacheKey
	cachedOps   op.Ops
	cachedMacro op.CallOp

	linearSaturations []uint8
	rankedSaturations []uint8
}

func (hm *Heatmap) computeBuckets() {
	hm.numYBuckets = int(math.Ceil(float64(hm.MaxY) / float64(hm.YBucketSize)))
	hm.data = make([]int, hm.numXBuckets*hm.numYBuckets)
	for _, xBuckets := range hm.origData {
		for i, y := range xBuckets {
			bin := y / hm.YBucketSize
			if bin >= hm.numYBuckets {
				// Say we have a bin size of 10, a minimum value of 0 and a maximum value of 100. Then we will have bins
				// [0, 10), [10, 20), ..., [90, 100]. That is, the last bucket is right closed, to catch the final
				// value. Otherwise we would need [90, 100) and [100, 100], and that'd be weird.
				//
				// Technically, our final bucket captures in this example is [100, ∞], because we'd rather have a catch
				// all than compute an invalid index that may write to other bins, or go out of bounds.
				bin = hm.numYBuckets - 1
			}
			idx := i*hm.numYBuckets + bin
			hm.data[idx]++
		}
	}
}

func (hm *Heatmap) computeSaturations() {
	if len(hm.data) == 0 {
		return
	}

	sorted := make([]int, len(hm.data))
	copy(sorted, hm.data)
	sort.Ints(sorted)
	prev := -1
	// We can reuse sorted's backing storage
	unique := sorted[:0]
	for _, v := range sorted {
		if v == prev {
			continue
		}
		unique = append(unique, v)
		prev = v
	}

	hm.linearSaturations = make([]uint8, len(hm.data))
	hm.rankedSaturations = make([]uint8, len(hm.data))
	for i, v := range hm.data {
		// OPT(dh): surely there's a way to structure this algorithm that we don't have to search our position in
		// the slice of unique, sorted buckets
		satIdx := sort.SearchInts(unique, v)
		if satIdx == len(unique) {
			panic("couldn't find bucket")
		}
		s := uint8(0xFF * (float32(satIdx+1) / float32(len(unique))))
		if s == 0 {
			// Ensure non-zero value has non-zero saturation
			s = 1
		}
		hm.rankedSaturations[i] = s

		s = uint8(0xFF * (float32(v) / float32(sorted[len(sorted)-1])))
		if s == 0 {
			// Ensure non-zero value has non-zero saturation
			s = 1
		}
		hm.linearSaturations[i] = s
	}
}

type HeatmapBucket struct {
	XStart time.Duration
	XEnd   time.Duration
	YStart int
	YEnd   int
	Count  int
}

func (hm *Heatmap) HoveredBucket() (HeatmapBucket, bool) {
	return hm.hovered, hm.hovered.Count != -1
}

func (hm *Heatmap) Layout(win *theme.Window, gtx layout.Context) layout.Dimensions {
	defer rtrace.StartRegion(context.Background(), "main.Heatmap.Layout").End()

	// TODO(dh): add scrollable X axis

	dims := gtx.Constraints.Max
	for {
		e, ok := gtx.Event(pointer.Filter{
			Target: hm,
			Kinds:  pointer.Move,
		})
		if !ok {
			break
		}
		ev := e.(pointer.Event)
		hm.pointer = ev.Position
		hm.pointerConstraint = dims
	}

	key := heatmapCacheKey{
		size:            dims,
		useLinearColors: hm.UseLinearColors,
		yBucketSize:     hm.YBucketSize,
		xBucketSize:     hm.XBucketSize,
	}

	if key.xBucketSize != hm.cacheKey.xBucketSize || key.yBucketSize != hm.cacheKey.yBucketSize {
		hm.numXBuckets = len(hm.origData[0])
		hm.computeBuckets()
		hm.computeSaturations()
	}

	numXBuckets := len(hm.data) / hm.numYBuckets
	xStepPx := float32(dims.X) / float32(numXBuckets)
	yStepPx := float32(dims.Y) / float32(hm.numYBuckets)

	if hm.cacheKey == key {
		hm.cachedMacro.Add(gtx.Ops)
	} else {
		hm.cacheKey = key
		hm.cachedOps.Reset()
		m := op.Record(&hm.cachedOps)

		stack := clip.Rect{Max: dims}.Push(&hm.cachedOps)
		// Use a white background, instead of the yellowish one we use everywhere else, to improve contrast and
		// legibility.
		theme.Fill(win, &hm.cachedOps, oklch(100, 0, 0))
		event.Op(&hm.cachedOps, hm)

		max := 0
		for _, v := range hm.data {
			if v > max {
				max = v
			}
		}

		// As per usual, batching draw calls hugely increases performance. Instead of thousands of draw calls, this caps us
		// at 256 draw calls, one per possible saturation.
		//
		// We don't bother reusing op.Ops or clip.Paths for now. We only hit this code when the window size has changed.
		// Otherwise we just reuse the previous frame's final output.
		var ops [256]op.Ops
		var paths [256]clip.Path
		for i := range paths {
			paths[i].Begin(&ops[i])
		}

		var saturations []uint8
		if hm.UseLinearColors {
			saturations = hm.linearSaturations
		} else {
			saturations = hm.rankedSaturations
		}

		for x := 0; x < numXBuckets; x++ {
			for y := 0; y < hm.numYBuckets; y++ {
				idx := x*hm.numYBuckets + y
				v := hm.data[idx]
				if v == 0 {
					// Don't explicitly draw rectangles for empty buckets. This is an optimization.
					continue
				}

				// Round coordinates to avoid conflation artifacts.
				xStart := round32(float32(x) * xStepPx)
				yEnd := round32(float32(dims.Y) - float32(y)*yStepPx)
				xEnd := round32(float32(x+1) * xStepPx)
				yStart := round32(float32(dims.Y) - float32(y+1)*yStepPx)

				p := &paths[saturations[idx]]
				p.MoveTo(f32.Pt(xStart, yStart))
				p.LineTo(f32.Pt(xEnd, yStart))
				p.LineTo(f32.Pt(xEnd, yEnd))
				p.LineTo(f32.Pt(xStart, yEnd))
				p.Close()
			}
		}

		for i := range paths {
			// We use a very simple color palette for our heatmap: 0 is white, max value is pure red, other values
			// are red with a lower saturation. We used to use our yellowish background color, where 0 was yellowish,
			// max value was pure red, and other values interpolated the hue between red–yellow and the saturation
			// between the background's saturation and 1. This was artistically pleasing, but had greatly reduced
			// legibility, both because of the reduced contrast and because the perceived intensity of the (hue,
			// saturation) pair wasn't intuitive.
			m := uint8(255 - i)
			c := color.NRGBA{0xFF, m, m, 0xFF}
			paint.FillShape(&hm.cachedOps, c, clip.Outline{Path: paths[i].End()}.Op())
		}

		stack.Pop()
		hm.cachedMacro = m.Stop()

		hm.cachedMacro.Add(gtx.Ops)
	}

	if hm.pointerConstraint == dims && hm.pointer.X > 0 && hm.pointer.Y > 0 && hm.pointer.X <= float32(dims.X) && hm.pointer.Y <= float32(dims.Y) {
		x := int(hm.pointer.X / xStepPx)
		y := int((float32(dims.Y) - hm.pointer.Y) / yStepPx)

		xStart := round32(float32(x) * xStepPx)
		yEnd := round32(float32(dims.Y) - float32(y)*yStepPx)
		xEnd := round32(float32(x+1) * xStepPx)
		yStart := round32(float32(dims.Y) - float32(y+1)*yStepPx)

		outline := myclip.RectangularOutline{
			Rect:  myclip.FRect{Min: f32.Pt(xStart, yStart), Max: f32.Pt(xEnd, yEnd)},
			Width: float32(gtx.Dp(1)),
		}.Op(gtx.Ops)
		// XXX use constant or theme for the color
		theme.FillShape(win, gtx.Ops, oklch(45.201, 0.31321, 264.05203), outline)

		idx := x*hm.numYBuckets + y
		hm.hovered = HeatmapBucket{
			XStart: time.Duration(x) * hm.XBucketSize,
			XEnd:   time.Duration(x)*hm.XBucketSize + hm.XBucketSize,
			YStart: y * hm.YBucketSize,
			YEnd:   y*hm.YBucketSize + hm.YBucketSize,
			Count:  hm.data[idx],
		}
	} else {
		hm.hovered = HeatmapBucket{Count: -1}
	}

	return layout.Dimensions{Size: gtx.Constraints.Max}
}

func (hm *Heatmap) SetData(data [][]int) {
	hm.origData = data
	hm.numXBuckets = len(data[0])
	// invalidate cache
	hm.cacheKey = heatmapCacheKey{}
}

type HeatmapComponent struct {
	trace *Trace
	hm    *Heatmap

	yStep     int
	useLinear widget.Bool
}

// bucketByX computes processor busyness for time intervals of size xStep.
// The returned value maps processor -> x bucket -> busy time.
func bucketByX(tr *Trace, xStep time.Duration) [][]int {
	buckets := make([][]int, len(tr.Processors))
	for i, p := range tr.Processors {
		buckets[i] = ptrace.ComputeProcessorBusy(tr.Trace, p, xStep)
	}
	return buckets
}

func NewHeatmapComponent(trace *Trace) *HeatmapComponent {
	const initialXStep = 100 * time.Millisecond
	const initialYStep = 1
	const maxY = 100
	hm := &Heatmap{
		UseLinearColors: false,
		XBucketSize:     initialXStep,
		YBucketSize:     initialYStep,
		MaxY:            maxY,
	}
	hm.SetData(bucketByX(trace, initialXStep))

	return &HeatmapComponent{
		trace: trace,
		hm:    hm,
	}
}

func (hmc *HeatmapComponent) Title() string {
	return "Processor utilization heatmap"
}

func (hmc *HeatmapComponent) Transition(theme.ComponentState) {
}

func (hmc *HeatmapComponent) WantsTransition(gtx layout.Context) theme.ComponentState {
	return theme.ComponentStateNone
}

func (hmc *HeatmapComponent) Layout(win *theme.Window, gtx layout.Context) layout.Dimensions {
	ySteps := [...]int{1, 2, 4, 5, 10, 20, 25, 50, 100}

	defer clip.Rect{Max: gtx.Constraints.Min}.Push(gtx.Ops).Pop()
	theme.Fill(win, gtx.Ops, win.Theme.Palette.Background)

	if hmc.useLinear.Update(gtx) {
		hmc.hm.UseLinearColors = hmc.useLinear.Value
	}

	for {
		e, ok := gtx.Event(
			key.FocusFilter{
				Target: hmc,
			},
			key.Filter{
				Focus: hmc,
				Name:  "↑",
			},
			key.Filter{
				Focus: hmc,
				Name:  "↓",
			},
			key.Filter{
				Focus: hmc,
				Name:  "←",
			},
			key.Filter{
				Focus: hmc,
				Name:  "→",
			},
		)
		if !ok {
			break
		}

		if ev, ok := e.(key.Event); ok && ev.State == key.Press {
			// TODO(dh): provide visual feedback, displaying the bucket size
			switch ev.Name {
			case "↑":
				hmc.yStep++
				if hmc.yStep >= len(ySteps) {
					hmc.yStep = len(ySteps) - 1
				}
				hmc.hm.YBucketSize = ySteps[hmc.yStep]
			case "↓":
				hmc.yStep--
				if hmc.yStep < 0 {
					hmc.yStep = 0
				}
				hmc.hm.YBucketSize = ySteps[hmc.yStep]
			case "←":
				hmc.hm.XBucketSize -= 10 * time.Millisecond
				if hmc.hm.XBucketSize < 10*time.Millisecond {
					hmc.hm.XBucketSize = 10 * time.Millisecond
				}
				hmc.hm.SetData(bucketByX(hmc.trace, hmc.hm.XBucketSize))
			case "→":
				hmc.hm.XBucketSize += 10 * time.Millisecond
				hmc.hm.SetData(bucketByX(hmc.trace, hmc.hm.XBucketSize))
			}
		}
	}

	event.Op(gtx.Ops, hmc)

	return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			return hmc.hm.Layout(win, gtx)
		}),
		// TODO(dh): add some padding between elements
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			var label string

			if b, ok := hmc.hm.HoveredBucket(); ok {
				close := ')'
				if b.YEnd >= hmc.hm.MaxY {
					close = ']'
				}
				label = local.Sprintf("time [%s, %s), range [%d, %d%c, count: %d", b.XStart, b.XEnd, b.YStart, b.YEnd, close, b.Count)
			}
			return theme.LineLabel(win.Theme, label).Layout(win, gtx)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			// TODO(dh): instead of using a checkbox, use a toggle switch that shows the two options (linear and
			// ranked). With the checkbox, the user doesn't know what's being used when the checkbox isn't
			// ticked.
			return theme.CheckBox(win.Theme, &hmc.useLinear, "Use linear saturation").Layout(win, gtx)
		}),
	)
}
