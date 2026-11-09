package runtimebrowser

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"math"

	"morde-runtime/runtimecore"
)

const (
	maxBrowserScreenshotMessageReserveBytes = 64 << 10
	maxBrowserScreenshotInputBytes          = (maxCDPMessageBytes - maxBrowserScreenshotMessageReserveBytes) * 3 / 4
	maxBrowserScreenshotWidth               = 1280
	maxBrowserScreenshotHeight              = 720
	maxBrowserScreenshotSourceDim           = 16384
	maxBrowserScreenshotSourcePx            = 32 << 20
)

func normalizeBrowserScreenshot(data []byte) ([]byte, error) {
	if len(data) < 4 || len(data) > maxBrowserScreenshotInputBytes || data[0] != 0xff || data[1] != 0xd8 ||
		data[len(data)-2] != 0xff || data[len(data)-1] != 0xd9 {
		return nil, runtimecore.ErrResultTooLarge
	}
	config, err := jpeg.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width < 1 || config.Width > maxBrowserScreenshotSourceDim ||
		config.Height < 1 || config.Height > maxBrowserScreenshotSourceDim ||
		int64(config.Width)*int64(config.Height) > maxBrowserScreenshotSourcePx {
		return nil, runtimecore.ErrResultTooLarge
	}
	source, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil || source.Bounds().Dx() != config.Width || source.Bounds().Dy() != config.Height {
		return nil, runtimecore.ErrResultTooLarge
	}
	if len(data) <= maxScreenshotBytes && config.Width <= maxBrowserScreenshotWidth && config.Height <= maxBrowserScreenshotHeight {
		return data, nil
	}

	width, height := fitBrowserScreenshot(config.Width, config.Height, maxBrowserScreenshotWidth, maxBrowserScreenshotHeight)
	for attempt := 0; attempt < 5; attempt++ {
		bounded := resizeBrowserScreenshot(source, width, height)
		for _, quality := range []int{80, 65, 50, 35} {
			var encoded bytes.Buffer
			if err := jpeg.Encode(&encoded, bounded, &jpeg.Options{Quality: quality}); err != nil {
				return nil, runtimecore.ErrResultTooLarge
			}
			if encoded.Len() <= maxScreenshotBytes {
				return encoded.Bytes(), nil
			}
		}
		width, height = max(1, width/2), max(1, height/2)
	}
	return nil, runtimecore.ErrResultTooLarge
}

func fitBrowserScreenshot(width, height, maxWidth, maxHeight int) (int, int) {
	if width <= maxWidth && height <= maxHeight {
		return width, height
	}
	if int64(width)*int64(maxHeight) >= int64(height)*int64(maxWidth) {
		return maxWidth, max(1, height*maxWidth/width)
	}
	return max(1, width*maxHeight/height), maxHeight
}

func resizeBrowserScreenshot(source image.Image, width, height int) *image.RGBA {
	bounds := source.Bounds()
	resized := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		sourceY := (float64(y)+0.5)*float64(bounds.Dy())/float64(height) - 0.5
		y0 := int(math.Floor(sourceY))
		fy := sourceY - float64(y0)
		if y0 < 0 {
			y0, fy = 0, 0
		}
		if y0 >= bounds.Dy() {
			y0, fy = bounds.Dy()-1, 0
		}
		y1 := min(y0+1, bounds.Dy()-1)
		sourceY0, sourceY1 := bounds.Min.Y+y0, bounds.Min.Y+y1
		for x := 0; x < width; x++ {
			sourceX := (float64(x)+0.5)*float64(bounds.Dx())/float64(width) - 0.5
			x0 := int(math.Floor(sourceX))
			fx := sourceX - float64(x0)
			if x0 < 0 {
				x0, fx = 0, 0
			}
			if x0 >= bounds.Dx() {
				x0, fx = bounds.Dx()-1, 0
			}
			x1 := min(x0+1, bounds.Dx()-1)
			sourceX0, sourceX1 := bounds.Min.X+x0, bounds.Min.X+x1
			c00 := color.RGBAModel.Convert(source.At(sourceX0, sourceY0)).(color.RGBA)
			c10 := color.RGBAModel.Convert(source.At(sourceX1, sourceY0)).(color.RGBA)
			c01 := color.RGBAModel.Convert(source.At(sourceX0, sourceY1)).(color.RGBA)
			c11 := color.RGBAModel.Convert(source.At(sourceX1, sourceY1)).(color.RGBA)
			blend := func(a, b, c, d uint8) uint8 {
				top := float64(a)*(1-fx) + float64(b)*fx
				bottom := float64(c)*(1-fx) + float64(d)*fx
				return uint8(top*(1-fy) + bottom*fy + 0.5)
			}
			resized.SetRGBA(x, y, color.RGBA{
				R: blend(c00.R, c10.R, c01.R, c11.R),
				G: blend(c00.G, c10.G, c01.G, c11.G),
				B: blend(c00.B, c10.B, c01.B, c11.B),
				A: blend(c00.A, c10.A, c01.A, c11.A),
			})
		}
	}
	return resized
}
