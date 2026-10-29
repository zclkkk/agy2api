package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
	"github.com/zclkkk/agy2api/internal/config"
	"github.com/zclkkk/agy2api/internal/protocol"
)

func imageModels(w http.ResponseWriter) {
	io.WriteString(w, `{"models":{"gemini-canvas":{"apiProvider":"API_PROVIDER_GOOGLE_GEMINI"},"gemini-test":{"apiProvider":"API_PROVIDER_GOOGLE_GEMINI"}},"imageGenerationModelIds":["gemini-canvas"]}`)
}

func pngImage(t *testing.T) []byte {
	t.Helper()
	im := image.NewNRGBA(image.Rect(0, 0, 8, 8))
	im.Set(0, 0, color.NRGBA{R: 255, A: 255})
	var out bytes.Buffer
	if err := png.Encode(&out, im); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func imageAnswer(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	w.Header().Set("Content-Type", "text/event-stream")
	frame(w, chunk([]ag.Part{{InlineData: &ag.Blob{MIMEType: "image/png", Data: base64.StdEncoding.EncodeToString(pngImage(t))}}}, "STOP", &ag.Usage{Input: 3, Output: 10, Total: 13, InputDetails: []ag.ModalityTokens{{Modality: "TEXT", Count: 3}}}))
}

func TestImagesGenerateBatchWithoutConversationState(t *testing.T) {
	var calls atomic.Int32
	svc, s := newService(t, nil, imageModels, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var in ag.Envelope
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Error(err)
			return
		}
		if in.Model != "gemini-canvas" || in.RequestType != ag.KindImage || len(in.Request.Tools) != 0 || len(in.Request.Contents) != 1 || len(in.Request.Contents[0].Parts) != 1 || in.Request.Contents[0].Parts[0].Text != "draw" {
			t.Errorf("bad native image request: %+v", in)
		}
		if strings.Join(in.Request.Generation.Modalities, ",") != "TEXT,IMAGE" {
			t.Error("image modalities missing")
		}
		imageAnswer(t, w)
	})
	// The Images API must not depend on replay state or store generated images.
	if err := svc.accounts[0].State.Close(); err != nil {
		t.Fatal(err)
	}
	res, body := send(t, s, "/v1/images/generations", `{"model":"gemini-canvas","prompt":"draw","n":2}`, nil)
	if res.StatusCode != 200 {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	var out protocol.ImagesResponse
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || len(out.Data) != 2 || out.Created == 0 || out.Size != "8x8" || out.OutputFormat != "png" || out.Usage == nil || out.Usage.Total != 26 {
		t.Fatalf("bad batch: calls=%d %+v", calls.Load(), out)
	}
	for _, item := range out.Data {
		data, err := base64.StdEncoding.DecodeString(item.Base64)
		if err != nil || !bytes.Equal(data, pngImage(t)) {
			t.Fatal("image bytes changed")
		}
	}
}

func multipartImages(t *testing.T, count int, fields [][2]string) (string, []byte) {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for _, field := range fields {
		if err := w.WriteField(field[0], field[1]); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < count; i++ {
		name := "image"
		if count > 1 {
			name = "image[]"
		}
		p, err := w.CreateFormFile(name, "input.png")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.Write(pngImage(t)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return w.FormDataContentType(), body.Bytes()
}

func TestImagesEditMultipartAndJSONURLs(t *testing.T) {
	var inputCount atomic.Int32
	svc, s := newService(t, nil, imageModels, func(w http.ResponseWriter, r *http.Request) {
		var in ag.Envelope
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Error(err)
			return
		}
		parts := in.Request.Contents[0].Parts
		if parts[0].Text != "make it blue" {
			t.Error("prompt lost")
		}
		inputCount.Store(int32(len(parts) - 1))
		for _, p := range parts[1:] {
			if p.InlineData == nil || p.InlineData.MIMEType != "image/png" || p.InlineData.Data != base64.StdEncoding.EncodeToString(pngImage(t)) {
				t.Error("image input lost")
			}
		}
		imageAnswer(t, w)
	})
	for _, n := range []int{1, 2} {
		contentType, data := multipartImages(t, n, [][2]string{{"model", "gemini-canvas"}, {"prompt", "make it blue"}, {"n", "1"}, {"output_format", "jpeg"}, {"output_compression", "80"}})
		res, out := send(t, s, "/v1/images/edits", string(data), map[string]string{"Content-Type": contentType})
		if res.StatusCode != 200 || inputCount.Load() != int32(n) {
			t.Fatalf("%d %s", res.StatusCode, out)
		}
		var result protocol.ImagesResponse
		if err := json.Unmarshal([]byte(out), &result); err != nil {
			t.Fatal(err)
		}
		b, _ := base64.StdEncoding.DecodeString(result.Data[0].Base64)
		_, format, err := image.Decode(bytes.NewReader(b))
		if err != nil || format != "jpeg" {
			t.Fatalf("JPEG output: %s %v", format, err)
		}
	}
	remote := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("X-Api-Key") != "" {
			t.Error("credentials sent to attachment host")
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(pngImage(t))
	}))
	defer remote.Close()
	svc.media = remote.Client()
	for _, url := range []string{remote.URL + "/image", "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngImage(t))} {
		body, _ := json.Marshal(map[string]any{"model": "gemini-canvas", "prompt": "make it blue", "images": []any{map[string]string{"image_url": url}}})
		res, out := send(t, s, "/v1/images/edits", string(body), nil)
		if res.StatusCode != 200 || inputCount.Load() != 1 {
			t.Fatalf("%d %s", res.StatusCode, out)
		}
	}
}

func TestImagesErrorsAndRequestLimits(t *testing.T) {
	_, s := newService(t, func(cfg *config.Config) { cfg.MaxBodyBytes = 1024 }, imageModels, func(w http.ResponseWriter, r *http.Request) {
		t.Error("invalid request reached generation")
	})
	for _, tc := range []struct {
		path, body, contentType string
		status                  int
	}{
		{"/v1/images/generations", `{"model":"gemini-test","prompt":"draw"}`, "application/json", 400},
		{"/v1/images/generations", `{"model":"gemini-canvas","prompt":"draw","stream":true}`, "application/json", 400},
		{"/v1/images/edits", `{"model":"gemini-canvas","prompt":"edit"}`, "application/json", 400},
		{"/v1/images/edits", `{"model":"gemini-canvas","prompt":"edit","images":[{"image_url":"data:text/plain;base64,YWJj"}]}`, "application/json", 400},
		{"/v1/images/edits", `{"model":"gemini-canvas","prompt":"edit","images":[{"image_url":"https://127.0.0.1/image"}]}`, "application/json", 400},
		{"/v1/images/generations", strings.Repeat("x", 1025), "application/json", 413},
		{"/v1/images/edits", "malformed", "multipart/form-data", 400},
	} {
		res, out := send(t, s, tc.path, tc.body, map[string]string{"Content-Type": tc.contentType})
		if res.StatusCode != tc.status {
			t.Fatalf("expected %d, got %d %s", tc.status, res.StatusCode, out)
		}
	}
	for _, fields := range [][][2]string{
		{{"model", "gemini-canvas"}, {"prompt", "edit"}, {"n", "invalid"}},
		{{"model", "gemini-canvas"}, {"model", "gemini-canvas"}, {"prompt", "edit"}},
	} {
		kind, body := multipartImages(t, 1, fields)
		res, out := send(t, s, "/v1/images/edits", string(body), map[string]string{"Content-Type": kind})
		if res.StatusCode != 400 {
			t.Fatalf("multipart validation: %d %s", res.StatusCode, out)
		}
	}
	kind, body := multipartImages(t, 1, [][2]string{{"model", "gemini-canvas"}, {"prompt", strings.Repeat("x", 1025)}})
	res, out := send(t, s, "/v1/images/edits", string(body), map[string]string{"Content-Type": kind})
	if res.StatusCode != 413 {
		t.Fatalf("multipart limit: %d %s", res.StatusCode, out)
	}
}

func TestImagesBatchFailureIsNotPartialSuccess(t *testing.T) {
	var calls atomic.Int32
	_, s := newService(t, nil, imageModels, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			imageAnswer(t, w)
			return
		}
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(429)
		io.WriteString(w, `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"busy"}}`)
	})
	res, out := send(t, s, "/v1/images/generations", `{"model":"gemini-canvas","prompt":"draw","n":2}`, nil)
	if res.StatusCode != 429 || res.Header.Get("Retry-After") != "30" || strings.Contains(out, "b64_json") {
		t.Fatalf("batch failure: %d %s", res.StatusCode, out)
	}
}

func TestImagesCancellationReleasesGenerationSlot(t *testing.T) {
	started := make(chan struct{})
	stopped := make(chan struct{})
	svc, s := newService(t, func(cfg *config.Config) { cfg.MaxConcurrentRequests = 1 }, imageModels, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
			t.Error("request context was not cancelled")
		}
		close(stopped)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, s.URL+"/v1/images/generations", strings.NewReader(`{"model":"gemini-canvas","prompt":"draw"}`))
	req.Header.Set("Content-Type", "application/json")
	done := make(chan struct{})
	go func() {
		defer close(done)
		res, _ := s.Client().Do(req)
		if res != nil {
			_ = res.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("generation did not start")
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream was not cancelled")
	}
	<-done
	deadline := time.Now().Add(5 * time.Second)
	for len(svc.accounts[0].slots) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(svc.accounts[0].slots) != 0 {
		t.Fatal("generation slot was leaked")
	}
}
