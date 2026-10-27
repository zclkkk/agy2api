package protocol

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"testing"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
)

func imageFixture(t *testing.T) *ag.Blob {
	t.Helper()
	im := image.NewNRGBA(image.Rect(0, 0, 2, 1))
	im.Set(0, 0, color.NRGBA{R: 255, A: 255})
	var out bytes.Buffer
	if err := png.Encode(&out, im); err != nil {
		t.Fatal(err)
	}
	return &ag.Blob{MIMEType: "image/png", Data: base64.StdEncoding.EncodeToString(out.Bytes())}
}

func TestImagesDecodeAndValidateCapabilities(t *testing.T) {
	for _, edit := range []bool{false, true} {
		base := map[string]any{"model": "gemini-canvas", "prompt": "draw a circle"}
		if edit {
			base["images"] = []any{map[string]string{"image_url": "data:image/png;base64," + imageFixture(t).Data}}
		}
		for name, fields := range map[string]map[string]any{
			"model": {"model": ""}, "prompt": {"prompt": "  "},
			"n zero": {"n": 0}, "n fraction": {"n": 1.5}, "n large": {"n": 11},
			"size": {"size": "1536x1024"}, "url output": {"response_format": "url"},
			"output format": {"output_format": "webp"}, "compression": {"output_compression": 50},
			"compression range": {"output_format": "jpeg", "output_compression": 101},
			"background":        {"background": "transparent"}, "opaque": {"background": "opaque"},
			"stream": {"stream": true}, "previews": {"partial_images": 1},
			"mask": {"mask": map[string]string{"image_url": "https://example.com/mask.png"}},
		} {
			t.Run(name, func(t *testing.T) {
				body := map[string]any{}
				for key, value := range base {
					body[key] = value
				}
				for key, value := range fields {
					body[key] = value
				}
				_, err := (Decoder{}).Images(raw(body), edit)
				var validation *Error
				if !errors.As(err, &validation) {
					t.Fatalf("expected validation error: %v", err)
				}
			})
		}
		in, err := (Decoder{}).Images(raw(base), edit)
		if err != nil || in.Count != 1 || in.OutputFormat != "png" || in.Request.Contents[0].Parts[0].Text != base["prompt"] || len(in.Warnings) != 0 {
			t.Fatalf("default request: %v %+v", err, in)
		}
		// Quality and policy hints still yield a valid image; they are reported.
		hinted := map[string]any{"quality": "high", "moderation": "low", "input_fidelity": "high", "style": "vivid"}
		for key, value := range base {
			hinted[key] = value
		}
		if hint, err := (Decoder{}).Images(raw(hinted), edit); err != nil || len(hint.Warnings) != 4 {
			t.Fatalf("hints: %v %v", err, hint.Warnings)
		}
		catalog := ag.Catalog{Models: map[string]json.RawMessage{"gemini-canvas": raw(map[string]any{}), "gemini-test": raw(map[string]any{})}, ImageModels: []string{"gemini-canvas"}}
		if err := in.ValidateModel(catalog); err != nil {
			t.Fatal(err)
		}
		in.Model = "gemini-test"
		if in.ValidateModel(catalog) == nil {
			t.Fatal("text model accepted for images")
		}
	}
}

func TestImagesInputReferences(t *testing.T) {
	url := "data:application/octet-stream;base64," + imageFixture(t).Data
	for _, refs := range []any{
		nil, []any{}, []any{map[string]string{"file_id": "file_1"}},
		[]any{map[string]string{"image_url": "http://example.com/a.png"}},
		[]any{map[string]string{"image_url": "gs://bucket/image.png"}},
		[]any{map[string]string{"image_url": "data:image/png;base64,??"}},
		make([]map[string]string, 17),
	} {
		if _, err := (Decoder{}).Images(raw(map[string]any{"model": "gemini-canvas", "prompt": "edit", "images": refs}), true); err == nil {
			t.Fatalf("invalid references accepted: %v", refs)
		}
	}
	body := raw(map[string]any{"model": "gemini-canvas", "prompt": "edit", "images": []any{map[string]string{"image_url": url}}})
	if _, err := (Decoder{}).Images(body, false); err == nil {
		t.Fatal("generation accepted edit inputs")
	}
	in, err := (Decoder{}).Images(body, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := in.ValidateInputs(); err != nil {
		t.Fatal(err)
	}
	if in.Request.Contents[0].Parts[1].InlineData.MIMEType != "image/png" {
		t.Fatal("untrusted MIME type retained")
	}
	in.Request.Contents[0].Parts[1].InlineData.Data = "aGVsbG8="
	if in.ValidateInputs() == nil {
		t.Fatal("non-image accepted")
	}
}

func TestImagesExplicitSizeMapsToNativeImageConfig(t *testing.T) {
	in, err := (Decoder{}).Images([]byte(`{"model":"gemini-canvas","prompt":"draw","size":"1024x1024","n":10}`), false)
	if err != nil {
		t.Fatal(err)
	}
	if in.Size != "1024x1024" || in.Count != 10 || in.Request.Generation.Image == nil || in.Request.Generation.Image.AspectRatio != "1:1" || in.Request.Generation.Image.Size != "1K" {
		t.Fatal("explicit image size was not mapped")
	}
}

func TestImagesWebPInputAndPNGConversion(t *testing.T) {
	// A one-pixel WebP image, independent of HTTP Content-Type or filename.
	const webp = "UklGRiIAAABXRUJQVlA4IBYAAAAwAQCdASoBAAEADsD+JaQAA3AAAAAA"
	in, err := (Decoder{}).Images(raw(map[string]any{
		"model": "gemini-canvas", "prompt": "edit",
		"images": []any{map[string]string{"image_url": "data:image/webp;base64," + webp}},
	}), true)
	if err != nil {
		t.Fatal(err)
	}
	if err := in.ValidateInputs(); err != nil {
		t.Fatal(err)
	}
	c := NewImageCollector(in)
	if err := c.Add(ag.Chunk{Parts: []ag.Part{{InlineData: &ag.Blob{Data: webp}}}, Finish: "STOP"}); err != nil {
		t.Fatal(err)
	}
	out, size, _, err := c.Finish()
	if err != nil {
		t.Fatal(err)
	}
	data, _ := base64.StdEncoding.DecodeString(out.Base64)
	if size != "1x1" || !bytes.HasPrefix(data, []byte("\x89PNG")) {
		t.Fatal("WebP conversion failed")
	}
}

func TestImageCollectorConversionsAndUsage(t *testing.T) {
	for _, format := range []string{"png", "jpeg"} {
		in := ImageIntent{OutputFormat: format, Compression: 100}
		c := NewImageCollector(in)
		if err := c.Add(ag.Chunk{Parts: []ag.Part{{Thought: true, InlineData: imageFixture(t)}, {InlineData: imageFixture(t)}}}); err != nil {
			t.Fatal(err)
		}
		if err := c.Add(ag.Chunk{Finish: "STOP", Usage: &ag.Usage{Input: 7, Output: 10, Thoughts: 2, Total: 19, InputDetails: []ag.ModalityTokens{{Modality: "TEXT", Count: 3}, {Modality: "IMAGE", Count: 4}}}}); err != nil {
			t.Fatal(err)
		}
		out, size, usage, err := c.Finish()
		if err != nil {
			t.Fatal(err)
		}
		data, err := base64.StdEncoding.DecodeString(out.Base64)
		if err != nil {
			t.Fatal(err)
		}
		img, actual, err := image.Decode(bytes.NewReader(data))
		if err != nil || actual != format || size != "2x1" {
			t.Fatalf("bad output: %s %s %v", actual, size, err)
		}
		if format == "jpeg" {
			r, g, b, _ := img.At(1, 0).RGBA()
			if r < 50000 || g < 50000 || b < 50000 {
				t.Fatal("JPEG transparency was not composited on white")
			}
		}
		if usage == nil || usage.InputDetails.Image != 4 || usage.Output != 12 || usage.Total != 19 {
			t.Fatalf("usage: %+v", usage)
		}
		response := ImagesResponse{}
		response.Append(out, size, usage)
		response.Append(out, size, &ImageUsage{Input: 7, InputDetails: ImageTokenDetails{Text: 3, Image: 4}, Output: 12, Total: 19})
		if len(response.Data) != 2 || response.Usage.Total != 38 || response.Usage.InputDetails.Image != 8 {
			t.Fatal("batch usage not accumulated")
		}
		response.Append(out, "3x2", nil)
		if response.Usage != nil || response.Size != "" {
			t.Fatal("incomplete metadata was fabricated")
		}
	}
}

func TestImageCollectorRequiresCompleteValidImage(t *testing.T) {
	for name, chunks := range map[string][]ag.Chunk{
		"no image":       {{Parts: []ag.Part{{Text: "cannot draw"}}, Finish: "STOP"}},
		"truncated":      {{Parts: []ag.Part{{InlineData: imageFixture(t)}}}},
		"max tokens":     {{Parts: []ag.Part{{InlineData: imageFixture(t)}}, Finish: "MAX_TOKENS"}},
		"safety":         {{Parts: []ag.Part{{InlineData: imageFixture(t)}}, Finish: "IMAGE_SAFETY"}},
		"multiple":       {{Parts: []ag.Part{{InlineData: imageFixture(t)}, {InlineData: imageFixture(t)}}, Finish: "STOP"}},
		"invalid base64": {{Parts: []ag.Part{{InlineData: &ag.Blob{Data: "??"}}}, Finish: "STOP"}},
		"invalid image":  {{Parts: []ag.Part{{InlineData: &ag.Blob{Data: "aGVsbG8="}}}, Finish: "STOP"}},
		"remote output":  {{Parts: []ag.Part{{FileData: &ag.File{URI: "https://example.com/out.png"}}}, Finish: "STOP"}},
	} {
		t.Run(name, func(t *testing.T) {
			c := NewImageCollector(ImageIntent{OutputFormat: "png"})
			var err error
			for _, chunk := range chunks {
				if err = c.Add(chunk); err != nil {
					break
				}
			}
			if err == nil {
				_, _, _, err = c.Finish()
			}
			if err == nil {
				t.Fatal("unusable generation succeeded")
			}
		})
	}
	c := NewImageCollector(ImageIntent{OutputFormat: "png", Size: "1024x1024"})
	if err := c.Add(ag.Chunk{Parts: []ag.Part{{InlineData: imageFixture(t)}}, Finish: "STOP"}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := c.Finish(); err == nil {
		t.Fatal("wrong pixel dimensions accepted")
	}
	c.options.Size = ""
	_, _, usage, err := c.Finish()
	if err != nil || usage != nil {
		t.Fatal("missing modality usage should be omitted")
	}
}
