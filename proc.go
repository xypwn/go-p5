// Copyright ©2020 The go-p5 Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package p5

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gioui.org/app"
	"gioui.org/f32"
	"gioui.org/font"
	"gioui.org/font/gofont"
	"gioui.org/gpu/headless"
	"gioui.org/io/event"
	"gioui.org/io/input"
	"gioui.org/io/key"
	"gioui.org/io/pointer"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget/material"
	"golang.org/x/exp/rand"
	"golang.org/x/image/bmp"
	"golang.org/x/image/tiff"
	"gonum.org/v1/gonum/spatial/r1"
)

const (
	defaultWidth  = 400
	defaultHeight = 400

	defaultFrameRate = 15 * time.Millisecond

	defaultSeed = 1
)

var (
	defaultBkgColor    = color.Transparent
	defaultFillColor   = color.White
	defaultStrokeColor = color.Black

	defaultTextColor = color.Black
	defaultTextSize  = float32(12)

	defaultTextFont font.Font
)

// gioWindow represents an operating system window operated by Gio.
type gioWindow interface {
	// Events returns the channel where events are delivered.
	Event() event.Event

	// Invalidate the window such that a FrameEvent will be generated immediately.
	// If the window is inactive, the event is sent when the window becomes active.
	Invalidate()
}

var _ gioWindow = (*app.Window)(nil)

// Proc is a p5 processor.
//
// Proc runs the bound Setup function once before the event loop.
// Proc then runs the bound Draw function once per event loop iteration.
type Proc struct {
	Setup Func
	Draw  Func
	Mouse Func

	ctl struct {
		FrameRate time.Duration

		mu           sync.RWMutex
		run          bool
		loop         bool
		nframes      uint64
		nscreenshots int
	}
	cfg struct {
		w int
		h int

		x    r1.Interval
		y    r1.Interval
		u2sX func(v float64) float64 // translate from user- to system coords
		u2sY func(v float64) float64 // translate from user- to system coords
		s2uX func(v float64) float64 // translate from system- to user coords
		s2uY func(v float64) float64 // translate from system- to user coords

		th *material.Theme
	}

	ctx  layout.Context
	stk  *stackOps
	head *headless.Window
	rand *rand.Rand

	newWindow func(opts ...app.Option) gioWindow
}

func newProc(w, h int) *Proc {
	proc := &Proc{
		ctx: layout.Context{
			Ops: new(op.Ops),
			Constraints: layout.Constraints{
				Max: image.Pt(w, h),
			},
		},
		rand: rand.New(rand.NewSource(defaultSeed)),

		newWindow: func(opts ...app.Option) gioWindow {
			a := new(app.Window)
			a.Option(opts...)
			return a
		},
	}
	proc.ctl.FrameRate = defaultFrameRate
	proc.ctl.loop = true
	proc.stk = newStackOps(proc.ctx.Ops)

	th := material.NewTheme()
	th.Shaper = text.NewShaper(text.WithCollection(gofont.Collection()))
	proc.cfg.th = th
	proc.initCanvas(w, h, defaultTextFont)
	proc.stk.cur().stroke.style.width = 2

	return proc
}

func (p *Proc) initCanvas(w, h int, fnt font.Font) {
	p.initCanvasDim(w, h, 0, float64(w), 0, float64(h))
	p.stk.cur().bkg = defaultBkgColor
	p.stk.cur().fill = defaultFillColor
	p.stk.cur().stroke.color = defaultStrokeColor

	p.stk.cur().text.color = defaultTextColor
	p.stk.cur().text.align = text.Start
	p.stk.cur().text.size = defaultTextSize
	p.stk.cur().text.font = fnt
}

func (p *Proc) initCanvasDim(w, h int, xmin, xmax, ymin, ymax float64) {
	p.cfg.w = w
	p.cfg.h = h
	p.cfg.x = r1.Interval{Min: xmin, Max: xmax}
	p.cfg.y = r1.Interval{Min: ymin, Max: ymax}

	var (
		wdx = 1 / (p.cfg.x.Max - p.cfg.x.Min) * float64(w)
		hdy = 1 / (p.cfg.y.Max - p.cfg.y.Min) * float64(h)

		dx = 1 / wdx
		dy = 1 / hdy
	)

	p.cfg.u2sX = func(v float64) float64 {
		return (v - p.cfg.x.Min) * wdx
	}

	p.cfg.s2uX = func(v float64) float64 {
		return (v * dx) + p.cfg.x.Min
	}

	p.cfg.u2sY = func(v float64) float64 {
		return (v - p.cfg.y.Min) * hdy
	}

	p.cfg.s2uY = func(v float64) float64 {
		return (v * dy) + p.cfg.y.Min
	}
}

func (p *Proc) cnvSize() (w, h float64) {
	w = float64(p.cfg.w)
	h = float64(p.cfg.h)
	return w, h
}

func (p *Proc) Run() {
	go func() {
		err := p.run()
		if err != nil {
			log.Fatalf("%+v", err)
		}
		os.Exit(0)
	}()
	app.Main()
}

func (p *Proc) run() error {
	p.setupUserFuncs()

	p.Setup()

	var (
		err    error
		width  = p.cfg.w
		height = p.cfg.h
	)

	w := p.newWindow(app.Title("p5"), app.Size(
		unit.Dp(float32(width)),
		unit.Dp(float32(height)),
	))
	p.head, err = headless.NewWindow(width, height)
	if err != nil {
		return fmt.Errorf("p5: could not create headless window: %w", err)
	}

	p.ctl.mu.Lock()
	p.ctl.run = true
	p.ctl.mu.Unlock()

	go func() {
		tck := time.NewTicker(p.ctl.FrameRate)
		defer tck.Stop()
		for range tck.C {
			w.Invalidate()
		}
	}()

	for {
		quit := false
		p.ctl.mu.RLock()
		quit = !p.ctl.run
		p.ctl.mu.RUnlock()
		if quit {
			return nil
		}

		switch e := w.Event().(type) {
		case app.DestroyEvent:
			return e.Err

		case app.FrameEvent:
			// The first frame should always been drawn, even if looping is disabled
			if p.IsLooping() || p.FrameCount() == 0 {
				p.draw(e)
			}
		}
	}
}

func (p *Proc) setupUserFuncs() {
	if p.Setup == nil {
		p.Setup = func() {}
	}
	if p.Draw == nil {
		p.Draw = func() {}
	}
	if p.Mouse == nil {
		p.Mouse = func() {}
	}
}

// This is needed for GioUI but never
// changed, so protections are not needed.
var inputEventTag = new(struct{})

func (p *Proc) handleInputEvents(source input.Source) {
	event.Op(p.ctx.Ops, inputEventTag)

	for {
		se, ok := source.Event(pointer.Filter{
			Target: inputEventTag,
			Kinds:  pointer.Press | pointer.Release | pointer.Move | pointer.Drag,
		}, key.Filter{})
		if !ok {
			break
		}

		switch ev := se.(type) {
		case key.Event:
			switch ev.Name {
			case key.NameEscape:
				p.ctl.mu.Lock()
				p.ctl.run = false
				p.ctl.mu.Unlock()
			case "F11":
				if ev.State == key.Press {
					p.ctl.mu.Lock()
					fname := fmt.Sprintf("out-%03d.png", p.ctl.nscreenshots)
					p.ctl.mu.Unlock()
					err := p.Screenshot(fname)
					if err != nil {
						log.Printf("could not take screenshot: %+v", err)
					}
					p.ctl.mu.Lock()
					p.ctl.nscreenshots++
					p.ctl.mu.Unlock()
				}
			}
		case pointer.Event:
			switch ev.Kind {
			case pointer.Press:
				Event.Mouse.Pressed = true
			case pointer.Release:
				Event.Mouse.Pressed = false
			case pointer.Move, pointer.Drag:
				Event.Mouse.PrevPosition = Event.Mouse.Position
			}
			Event.Mouse.Position.X = p.cfg.s2uX(float64(ev.Position.X))
			Event.Mouse.Position.Y = p.cfg.s2uY(float64(ev.Position.Y))
			Event.Mouse.Buttons = Buttons(ev.Buttons)
		}
	}

}

func (p *Proc) draw(e app.FrameEvent) {
	p.incFrameCount()
	p.ctx = app.NewContext(p.ctx.Ops, e)

	ops := p.ctx.Ops

	// Required so that mouse event positions are reported
	// properly on platforms that use custom frame decoration.
	globalClip := clip.Rect{Max: e.Size}.Push(ops)

	clr := rgba(p.stk.cur().bkg)
	paint.Fill(ops, clr)

	p.handleInputEvents(e.Source)
	p.Draw()
	globalClip.Pop()

	e.Frame(ops)
}

func (p *Proc) pt(x, y float64) f32.Point {
	return f32.Point{
		X: float32(p.cfg.u2sX(x)),
		Y: float32(p.cfg.u2sY(y)),
	}
}

func rgba(c color.Color) color.NRGBA {
	r, g, b, a := c.RGBA()
	return color.NRGBA{R: uint8(r), G: uint8(g), B: uint8(b), A: uint8(a)}
}

// Canvas defines the dimensions of the painting area, in pixels.
func (p *Proc) Canvas(w, h int) {
	p.initCanvasDim(w, h, 0, float64(w), 0, float64(h))
}

// PhysCanvas sets the dimensions of the painting area, in pixels, and
// associates physical quantities.
func (p *Proc) PhysCanvas(w, h int, xmin, xmax, ymin, ymax float64) {
	p.initCanvasDim(w, h, xmin, xmax, ymin, ymax)
}

// Background defines the background color for the painting area.
// The default color is transparent.
func (p *Proc) Background(c color.Color) {
	p.stk.cur().bkg = c
}

func (p *Proc) doStroke() bool {
	return p.stk.cur().stroke.color != nil &&
		p.stk.cur().stroke.style.width > 0
}

// Stroke sets the color of the strokes.
func (p *Proc) Stroke(c color.Color) {
	p.stk.cur().stroke.color = c
}

// StrokeWidth sets the size of the strokes.
func (p *Proc) StrokeWidth(v float64) {
	p.stk.cur().stroke.style.width = float32(v)
}

func (p *Proc) doFill() bool {
	return p.stk.cur().fill != nil
}

// Fill sets the color used to fill shapes.
func (p *Proc) Fill(c color.Color) {
	p.stk.cur().fill = c
}

// LoadFonts sets the fonts collection to use for text.
func (p *Proc) LoadFonts(fnt []font.FontFace) {
	th := material.NewTheme()
	th.Shaper = text.NewShaper(text.WithCollection(gofont.Collection()))
	p.cfg.th = th
}

// TextSize sets the text size.
func (p *Proc) TextSize(size float64) {
	p.stk.cur().text.size = float32(size)
}

func (p *Proc) TextFont(fnt font.Font) {
	p.stk.cur().text.font = fnt
}

// Text draws txt on the screen at (x,y).
func (p *Proc) Text(txt string, x, y float64) {
	x = p.cfg.u2sX(x)
	y = p.cfg.u2sY(y)

	var (
		offset = x
		w, _   = p.cnvSize()
		size   = p.stk.cur().text.size
	)
	switch p.stk.cur().text.align {
	case text.End:
		offset = x - w
	case text.Middle:
		offset = x - 0.5*w
	}
	defer op.TransformOp{}.Push(p.ctx.Ops).Pop()
	op.Affine(f32.Affine2D{}.Offset(f32.Point{
		X: float32(offset),
		Y: float32(y) - size,
	})).Add(p.ctx.Ops) // shift to use baseline

	l := material.Label(p.cfg.th, unit.Sp(size), txt)
	l.Color = rgba(p.stk.cur().text.color)
	l.Alignment = p.stk.cur().text.align
	l.Font = p.stk.cur().text.font
	l.Layout(p.ctx)
}

// Screenshot saves the current canvas to the provided file.
// Supported file formats are: PNG, JPEG and GIF.
func (p *Proc) Screenshot(fname string) error {
	err := p.head.Frame(p.ctx.Ops)
	if err != nil {
		return fmt.Errorf("p5: could not run headless frame: %w", err)
	}

	img := image.NewRGBA(image.Rect(0, 0, p.cfg.w, p.cfg.h))
	err = p.head.Screenshot(img)
	if err != nil {
		return fmt.Errorf("p5: could not take screenshot: %w", err)
	}

	f, err := os.Create(fname)
	if err != nil {
		return fmt.Errorf("p5: could not create screenshot file: %w", err)
	}
	defer f.Close()

	var encode func(io.Writer, image.Image) error
	switch ext := filepath.Ext(fname); strings.ToLower(ext) {
	case ".jpeg", ".jpg":
		encode = func(w io.Writer, img image.Image) error {
			return jpeg.Encode(w, img, nil)
		}
	case ".gif":
		encode = func(w io.Writer, img image.Image) error {
			return gif.Encode(w, img, nil)
		}
	case ".png":
		encode = png.Encode
	default:
		log.Printf("unknown file extension %q. using png", ext)
		encode = png.Encode
	}

	err = encode(f, img)
	if err != nil {
		return fmt.Errorf("p5: could not encode screenshot: %w", err)
	}

	err = f.Close()
	if err != nil {
		return fmt.Errorf("p5: could not save screenshot: %w", err)
	}

	return nil
}

// RandomSeed changes the sequence of numbers generated by Random.
func (p *Proc) RandomSeed(seed uint64) {
	p.rand.Seed(seed)
}

// Random returns a pseudo-random number in [min,max).
// Random will produce the same sequence of numbers every time the program runs.
// Use RandomSeed with a seed that changes (like time.Now().UnixNano()) in order to
// produce different sequences of numbers.
func (p *Proc) Random(min, max float64) float64 {
	return p.rand.Float64()*(max-min) + min
}

// RandomGaussian returns a random number following a Gaussian distribution with the provided
// mean and standard deviation.
func (p *Proc) RandomGaussian(mean, stdDev float64) float64 {
	return p.rand.NormFloat64()*stdDev + mean
}

func (p *Proc) incFrameCount() {
	p.ctl.mu.Lock()
	defer p.ctl.mu.Unlock()
	p.ctl.nframes++
}

// FrameCount returns the number of frames that have been displayed since the program started.
func (p *Proc) FrameCount() uint64 {
	p.ctl.mu.RLock()
	defer p.ctl.mu.RUnlock()
	return p.ctl.nframes
}

// By default, p5 continuously executes the code within draw().
// Loop starts the draw loop again, if it was stopped previously by calling NoLoop().
func (p *Proc) Loop() {
	p.ctl.mu.Lock()
	defer p.ctl.mu.Unlock()
	p.ctl.loop = true
}

// NoLoop stops p5 from continuously executing the code within draw().
func (p *Proc) NoLoop() {
	p.ctl.mu.Lock()
	defer p.ctl.mu.Unlock()
	p.ctl.loop = false
}

// IsLooping checks if p5 is continuously executing the code within draw() or not.
func (p *Proc) IsLooping() bool {
	p.ctl.mu.RLock()
	defer p.ctl.mu.RUnlock()
	return p.ctl.loop
}

// ReadImage reads a BMP, JPG, GIF, PNG or TIFF image from the provided path.
func (p *Proc) ReadImage(fname string) (img image.Image, err error) {
	raw, err := os.ReadFile(fname)
	if err != nil {
		return nil, fmt.Errorf("p5: could not read image at %q: %w", fname, err)
	}
	r := bytes.NewReader(raw)

	switch {
	case bytes.Equal(raw[:4], []byte("\x89PNG")):
		img, err = png.Decode(r)
	case bytes.Equal(raw[:3], []byte("\xff\xd8\xff")):
		img, err = jpeg.Decode(r)
	case bytes.Equal(raw[:6], []byte("GIF87a")), bytes.Equal(raw[:6], []byte("GIF89a")):
		img, err = gif.Decode(r)
	case bytes.Equal(raw[:2], []byte("BM")):
		img, err = bmp.Decode(r)
	case bytes.Equal(raw[:4], []byte("II\x2A\x00")), bytes.Equal(raw[:4], []byte("MM\x00\x2A")):
		img, err = tiff.Decode(r)
	default:
		err = fmt.Errorf("p5: unknown image header for %q (hdr=%q)", fname, raw[:4])
	}
	return img, err
}

// DrawImage draws the provided image at (x,y).
func (p *Proc) DrawImage(img image.Image, x, y float64) {
	p.stk.push()
	defer p.stk.pop()

	p.stk.translate(x, y)
	paint.NewImageOp(img).Add(p.stk.ops)
	paint.PaintOp{}.Add(p.stk.ops)
}
