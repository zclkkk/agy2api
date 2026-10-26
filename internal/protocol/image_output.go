package protocol

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"strings"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
	_ "golang.org/x/image/webp" // Images edits accept WebP input.
)

const maxImagePixels = 32_000_000
const MaxImageOutputBytes = 64 << 20

type ImageData struct {
	Base64 string `json:"b64_json"`
}
type ImageTokenDetails struct {
	Text  int `json:"text_tokens"`
	Image int `json:"image_tokens"`
}
type ImageUsage struct {
	Input        int               `json:"input_tokens"`
	InputDetails ImageTokenDetails `json:"input_tokens_details"`
	Output       int               `json:"output_tokens"`
	Total        int               `json:"total_tokens"`
}
type ImagesResponse struct {
	Created      int64       `json:"created"`
	Data         []ImageData `json:"data"`
	OutputFormat string      `json:"output_format"`
	Size         string      `json:"size,omitempty"`
	Usage        *ImageUsage `json:"usage,omitempty"`
}

func imageConfig(data []byte) (image.Config, string, error) {
	c, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || (format != "png" && format != "jpeg" && format != "webp") {
		return c, format, errors.New("expected a valid PNG, JPEG or WebP image")
	}
	if c.Width <= 0 || c.Height <= 0 || int64(c.Width)*int64(c.Height) > maxImagePixels {
		return c, format, errors.New("image exceeds the 32-megapixel limit")
	}
	return c, format, nil
}

// ValidateInputs checks decoded dimensions before forwarding attachments, and
// derives their MIME types from the bytes instead of trusting upload headers.
func (in *ImageIntent) ValidateInputs() error {
	for _, content := range in.Request.Contents {
		for _, part := range content.Parts {
			if part.InlineData == nil {
				continue
			}
			data, err := base64.StdEncoding.DecodeString(part.InlineData.Data)
			if err != nil {
				return invalid("images", "invalid base64 image")
			}
			_, format, err := imageConfig(data)
			if err != nil {
				return invalid("images", err.Error())
			}
			part.InlineData.MIMEType = "image/" + format
		}
	}
	return nil
}

// ImageCollector consumes one complete upstream generation. No image is
// returned until the stream and its finish reason have both been validated.
type ImageCollector struct {
	options ImageIntent
	image   *ag.Blob
	finish  string
	bytes   int
	usage   ag.Usage
}

func NewImageCollector(options ImageIntent) *ImageCollector {
	return &ImageCollector{options: options}
}

func (c *ImageCollector) Add(chunk ag.Chunk) error {
	if chunk.Usage != nil {
		u := chunk.Usage
		c.usage.Input = max(c.usage.Input, u.Input)
		c.usage.Output = max(c.usage.Output, u.Output)
		c.usage.Thoughts = max(c.usage.Thoughts, u.Thoughts)
		c.usage.Total = max(c.usage.Total, u.Total)
		if len(u.InputDetails) > 0 {
			c.usage.InputDetails = u.InputDetails
		}
	}
	if chunk.Finish != "" {
		c.finish = chunk.Finish
	}
	for _, part := range chunk.Parts {
		c.bytes += len(part.Text) + len(part.Signature)
		if part.InlineData != nil {
			c.bytes += len(part.InlineData.Data)
		}
		if c.bytes > MaxImageOutputBytes {
			return errors.New("upstream image output exceeds 64 MiB")
		}
		if part.FunctionCall != nil || part.FunctionResponse != nil || part.FileData != nil {
			return errors.New("unexpected upstream content in image generation")
		}
		if !part.Thought && part.InlineData != nil {
			if c.image != nil {
				return errors.New("upstream returned multiple images for a single-image generation")
			}
			c.image = part.InlineData
		}
	}
	return nil
}

func (c *ImageCollector) Finish() (ImageData, string, *ImageUsage, error) {
	switch strings.ToUpper(c.finish) {
	case "STOP", "FINISH_REASON_UNSPECIFIED":
	case "SAFETY", "IMAGE_SAFETY", "IMAGE_PROHIBITED_CONTENT", "PROHIBITED_CONTENT", "BLOCKLIST":
		return ImageData{}, "", nil, &ag.HTTPError{Status: 400, Message: "image generation was blocked by the upstream safety policy"}
	default:
		return ImageData{}, "", nil, errors.New("upstream did not complete image generation")
	}
	if c.image == nil {
		return ImageData{}, "", nil, errors.New("upstream completed without an image")
	}
	data, err := base64.StdEncoding.DecodeString(c.image.Data)
	if err != nil {
		return ImageData{}, "", nil, errors.New("upstream returned invalid base64 image data")
	}
	config, format, err := imageConfig(data)
	if err != nil {
		return ImageData{}, "", nil, err
	}
	size := fmt.Sprintf("%dx%d", config.Width, config.Height)
	if c.options.Size != "" && c.options.Size != size {
		return ImageData{}, "", nil, errors.New("upstream image dimensions do not match the requested size")
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return ImageData{}, "", nil, errors.New("upstream returned a corrupt image")
	}
	if c.options.OutputFormat != format || format == "jpeg" {
		var out bytes.Buffer
		if c.options.OutputFormat == "jpeg" {
			// JPEG has no alpha channel. Composite transparency on white.
			opaque := image.NewRGBA(img.Bounds())
			draw.Draw(opaque, opaque.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
			draw.Draw(opaque, opaque.Bounds(), img, img.Bounds().Min, draw.Over)
			err = jpeg.Encode(&out, opaque, &jpeg.Options{Quality: max(1, c.options.Compression)})
		} else {
			err = png.Encode(&out, img)
		}
		if err != nil {
			return ImageData{}, "", nil, err
		}
		data = out.Bytes()
	}
	if base64.StdEncoding.EncodedLen(len(data)) > MaxImageOutputBytes {
		return ImageData{}, "", nil, errors.New("encoded image exceeds 64 MiB")
	}
	return ImageData{Base64: base64.StdEncoding.EncodeToString(data)}, size, c.imageUsage(), nil
}

func (c *ImageCollector) imageUsage() *ImageUsage {
	u := c.usage
	if len(u.InputDetails) == 0 {
		// Images usage requires an input modality breakdown. Omit optional
		// usage when the upstream has not supplied it, rather than invent it.
		return nil
	}
	details := ImageTokenDetails{}
	for _, part := range u.InputDetails {
		if part.Count < 0 {
			return nil
		}
		switch part.Modality {
		case "TEXT":
			details.Text += part.Count
		case "IMAGE":
			details.Image += part.Count
		default:
			return nil
		}
	}
	if details.Text+details.Image != u.Input || u.Total != u.Input+u.Output+u.Thoughts {
		return nil
	}
	return &ImageUsage{Input: u.Input, InputDetails: details, Output: u.Output + u.Thoughts, Total: u.Total}
}

func (r *ImagesResponse) Append(data ImageData, size string, usage *ImageUsage) {
	if len(r.Data) == 0 {
		r.Size, r.Usage = size, usage
	} else {
		if r.Size != size {
			r.Size = ""
		}
		if r.Usage == nil || usage == nil {
			r.Usage = nil
		} else {
			r.Usage.Input += usage.Input
			r.Usage.Output += usage.Output
			r.Usage.Total += usage.Total
			r.Usage.InputDetails.Text += usage.InputDetails.Text
			r.Usage.InputDetails.Image += usage.InputDetails.Image
		}
	}
	r.Data = append(r.Data, data)
}
