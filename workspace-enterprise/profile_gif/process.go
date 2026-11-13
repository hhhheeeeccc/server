package profile_gif

import (
	"bytes"
	"errors"
	"image"
	"image/color/palette"
	"image/draw"
	"image/gif"

	"github.com/mattermost/mattermost/server/v8/channels/app/imaging"
	"github.com/mattermost/mattermost/server/v8/channels/utils/imgutils"
)

const (
	MaxSize        = 20 * 1024 * 1024
	MaxFrames      = 500
	MaxTotalPixels = 100 * 1024 * 1024
	ProfileSize    = 128
)

var ErrLimit = errors.New("profile GIF exceeds size, frame, or pixel limit")

// Process validates and resizes every GIF frame while preserving animation timing.
func Process(data []byte) ([]byte, error) {
	if len(data) > MaxSize {
		return nil, ErrLimit
	}
	config, err := gif.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	frameCount, err := imgutils.CountGIFFrames(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if frameCount < 1 || frameCount > MaxFrames ||
		int64(config.Width)*int64(config.Height)*int64(frameCount) > MaxTotalPixels {
		return nil, ErrLimit
	}
	animated, err := gif.DecodeAll(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}

	canvas := image.NewRGBA(image.Rect(0, 0, config.Width, config.Height))
	frames := make([]*image.Paletted, 0, len(animated.Image))
	for index, frame := range animated.Image {
		var previous *image.RGBA
		if index < len(animated.Disposal) && animated.Disposal[index] == gif.DisposalPrevious {
			previous = image.NewRGBA(canvas.Bounds())
			copy(previous.Pix, canvas.Pix)
		}

		draw.Draw(canvas, frame.Bounds(), frame, frame.Bounds().Min, draw.Over)
		resized := imaging.FillCenter(canvas, ProfileSize, ProfileSize)
		paletted := image.NewPaletted(image.Rect(0, 0, ProfileSize, ProfileSize), palette.Plan9)
		draw.FloydSteinberg.Draw(paletted, paletted.Bounds(), resized, image.Point{})
		frames = append(frames, paletted)

		if index < len(animated.Disposal) {
			switch animated.Disposal[index] {
			case gif.DisposalBackground:
				draw.Draw(canvas, frame.Bounds(), image.Transparent, image.Point{}, draw.Src)
			case gif.DisposalPrevious:
				copy(canvas.Pix, previous.Pix)
			}
		}
	}

	animated.Image = frames
	animated.Config.Width = ProfileSize
	animated.Config.Height = ProfileSize
	animated.Disposal = nil // Every output frame contains the complete canvas.
	var output bytes.Buffer
	if err := gif.EncodeAll(&output, animated); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}
