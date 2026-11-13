package profile_gif

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProcessPreservesAnimation(t *testing.T) {
	colors := color.Palette{color.Black, color.White}
	first := image.NewPaletted(image.Rect(0, 0, 4, 4), colors)
	second := image.NewPaletted(image.Rect(0, 0, 4, 4), colors)
	for i := range second.Pix {
		second.Pix[i] = 1
	}
	var input bytes.Buffer
	require.NoError(t, gif.EncodeAll(&input, &gif.GIF{
		Image:     []*image.Paletted{first, second},
		Delay:     []int{5, 10},
		LoopCount: 2,
		Config:    image.Config{Width: 4, Height: 4},
	}))

	output, err := Process(input.Bytes())
	require.NoError(t, err)
	result, err := gif.DecodeAll(bytes.NewReader(output))
	require.NoError(t, err)
	require.Len(t, result.Image, 2)
	require.Equal(t, []int{5, 10}, result.Delay)
	require.Equal(t, 2, result.LoopCount)
	require.Equal(t, ProfileSize, result.Config.Width)
	require.Equal(t, ProfileSize, result.Config.Height)
	require.NotEqual(t, result.Image[0].Pix, result.Image[1].Pix)
}

func TestProcessRejectsOversize(t *testing.T) {
	_, err := Process(make([]byte, MaxSize+1))
	require.ErrorIs(t, err, ErrLimit)
}
