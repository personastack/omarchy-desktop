package desktopexecutor

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	_ "image/jpeg"
	_ "image/png"
	"strconv"

	"github.com/personastack/personastack-api/pkg/client/agentgatewayruntime"
)

const (
	maxCuaImageBytes  = 5 << 20
	maxCuaImagePixels = 80_000_000
)

// boundCuaResult keeps screenshot payloads below the Gateway frame limit and
// updates Cua's screenshot dimensions when a returned image is resized.
func boundCuaResult(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	if ctx == nil || ctx.Err() != nil {
		return nil, ErrUnavailable
	}
	var result map[string]json.RawMessage
	if !json.Valid(raw) || json.Unmarshal(raw, &result) != nil || result == nil {
		return nil, ErrUnavailable
	}
	var content []json.RawMessage
	if len(result["content"]) == 0 || json.Unmarshal(result["content"], &content) != nil {
		return raw, nil
	}
	indices := make([]int, 0)
	for index, encoded := range content {
		var block map[string]json.RawMessage
		if json.Unmarshal(encoded, &block) != nil || rawString(block["type"]) != "image" {
			continue
		}
		indices = append(indices, index)
	}
	if len(indices) == 0 {
		return raw, nil
	}
	perImageLimit := maxCuaImageBytes / len(indices)
	var screenshotWidth, screenshotHeight int
	var screenshotScale float64
	for _, index := range indices {
		if ctx.Err() != nil {
			return nil, ErrUnavailable
		}
		var block map[string]json.RawMessage
		if json.Unmarshal(content[index], &block) != nil {
			return nil, ErrUnavailable
		}
		encoded := rawString(block["data"])
		if encoded == "" {
			return nil, ErrUnavailable
		}
		if len(encoded) <= perImageLimit {
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, ErrUnavailable
		}
		config, _, err := image.DecodeConfig(bytes.NewReader(decoded))
		if err != nil || config.Width < 1 || config.Height < 1 || config.Width > maxCuaImagePixels/config.Height {
			return nil, ErrUnavailable
		}
		source, _, err := image.Decode(bytes.NewReader(decoded))
		if err != nil {
			return nil, ErrUnavailable
		}
		var replacement []byte
		for _, scale := range []float64{1, .75, .5, .25, .125} {
			if ctx.Err() != nil {
				return nil, ErrUnavailable
			}
			scaled := scaleImage(source, scale)
			for _, quality := range []int{70, 50, 30} {
				var buffer bytes.Buffer
				if err := encodeJPEGContext(ctx, &buffer, scaled, quality); err != nil {
					if ctx.Err() != nil {
						return nil, ErrUnavailable
					}
					continue
				}
				if buffer.Len() <= perImageLimit/4*3 {
					replacement = append([]byte(nil), buffer.Bytes()...)
					screenshotWidth, screenshotHeight = scaled.Bounds().Dx(), scaled.Bounds().Dy()
					screenshotScale = float64(screenshotWidth) / float64(config.Width)
					break
				}
			}
			if replacement != nil {
				break
			}
		}
		if replacement == nil {
			return nil, ErrUnavailable
		}
		data, _ := json.Marshal(base64.StdEncoding.EncodeToString(replacement))
		mimeType, _ := json.Marshal("image/jpeg")
		block["data"], block["mimeType"] = data, mimeType
		content[index], _ = json.Marshal(block)
	}
	contentJSON, err := json.Marshal(content)
	if err != nil {
		return nil, ErrUnavailable
	}
	result["content"] = contentJSON
	if screenshotScale > 0 {
		var structured map[string]json.RawMessage
		if json.Unmarshal(result["structuredContent"], &structured) == nil && structured != nil && structured["screenshot_width"] != nil && structured["screenshot_height"] != nil {
			width, _ := json.Marshal(screenshotWidth)
			height, _ := json.Marshal(screenshotHeight)
			mimeType, _ := json.Marshal("image/jpeg")
			structured["screenshot_width"], structured["screenshot_height"], structured["screenshot_mime_type"] = width, height, mimeType
			if len(structured["scale_factor"]) > 0 {
				prior, err := strconv.ParseFloat(string(structured["scale_factor"]), 64)
				if err == nil {
					factor, _ := json.Marshal(prior * screenshotScale)
					structured["scale_factor"] = factor
				}
			}
			result["structuredContent"], _ = json.Marshal(structured)
		}
	}
	bounded, err := json.Marshal(result)
	if err != nil || len(bounded) > agentgatewayruntime.DesktopControlFrameLimit-1024 {
		return nil, fmt.Errorf("Cua result exceeds the Desktop Control frame limit: %w", ErrUnavailable)
	}
	return bounded, nil
}

type scaledScreenshot struct {
	source image.Image
	scale  float64
}

func scaleImage(source image.Image, scale float64) image.Image {
	if scale >= 1 {
		return source
	}
	return scaledScreenshot{source: source, scale: scale}
}

func (s scaledScreenshot) ColorModel() color.Model { return color.RGBAModel }

func (s scaledScreenshot) Bounds() image.Rectangle {
	width := max(1, int(float64(s.source.Bounds().Dx())*s.scale))
	height := max(1, int(float64(s.source.Bounds().Dy())*s.scale))
	return image.Rect(0, 0, width, height)
}

func (s scaledScreenshot) At(x, y int) color.Color {
	if !(image.Point{X: x, Y: y}.In(s.Bounds())) {
		return color.RGBA{}
	}
	bounds := s.source.Bounds()
	outputBounds := s.Bounds()
	scaleX := float64(bounds.Dx()) / float64(outputBounds.Dx())
	scaleY := float64(bounds.Dy()) / float64(outputBounds.Dy())
	left, right := float64(x)*scaleX, float64(x+1)*scaleX
	top, bottom := float64(y)*scaleY, float64(y+1)*scaleY
	var red, green, blue, weightTotal float64
	for sourceY := int(top); sourceY < int(bottom+1); sourceY++ {
		weightY := min(bottom, float64(sourceY+1)) - max(top, float64(sourceY))
		if weightY <= 0 || sourceY < 0 || sourceY >= bounds.Dy() {
			continue
		}
		for sourceX := int(left); sourceX < int(right+1); sourceX++ {
			weightX := min(right, float64(sourceX+1)) - max(left, float64(sourceX))
			if weightX <= 0 || sourceX < 0 || sourceX >= bounds.Dx() {
				continue
			}
			weight := weightX * weightY
			pixel := color.RGBAModel.Convert(s.source.At(bounds.Min.X+sourceX, bounds.Min.Y+sourceY)).(color.RGBA)
			red += float64(int(pixel.R)+255-int(pixel.A)) * weight
			green += float64(int(pixel.G)+255-int(pixel.A)) * weight
			blue += float64(int(pixel.B)+255-int(pixel.A)) * weight
			weightTotal += weight
		}
	}
	if weightTotal == 0 {
		return color.RGBA{A: 255}
	}
	return color.RGBA{R: uint8(red / weightTotal), G: uint8(green / weightTotal), B: uint8(blue / weightTotal), A: 255}
}

type imageCancelSignal struct{}

type contextImage struct {
	image.Image
	ctx   context.Context
	reads uint64
}

func (i *contextImage) At(x, y int) color.Color {
	if i.reads%1024 == 0 && i.ctx.Err() != nil {
		panic(imageCancelSignal{})
	}
	i.reads++
	return i.Image.At(x, y)
}

func encodeJPEGContext(ctx context.Context, buffer *bytes.Buffer, source image.Image, quality int) (err error) {
	if ctx == nil || ctx.Err() != nil {
		return context.Canceled
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			if _, canceled := recovered.(imageCancelSignal); canceled {
				err = ctx.Err()
				return
			}
			panic(recovered)
		}
	}()
	return jpeg.Encode(buffer, &contextImage{Image: source, ctx: ctx}, &jpeg.Options{Quality: quality})
}

func rawString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}
