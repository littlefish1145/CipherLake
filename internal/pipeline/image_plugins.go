package pipeline

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"

	"golang.org/x/image/draw"
	"golang.org/x/image/webp"
)

const (
	maxImageInputBytes = 50 * 1024 * 1024
	maxImagePixels     = 5000 * 5000
)

type RealImageCompressPlugin struct {
	Quality int
	Format  string
}

func NewRealImageCompressPlugin(quality int, format string) *RealImageCompressPlugin {
	if quality <= 0 || quality > 100 {
		quality = 80
	}
	if format == "" {
		format = "jpeg"
	}
	return &RealImageCompressPlugin{
		Quality: quality,
		Format:  format,
	}
}

func (p *RealImageCompressPlugin) Name() string { return "image_compress" }

func (p *RealImageCompressPlugin) Process(ctx context.Context, input *ObjectInput) (*ProcessResult, error) {
	if !isImageType(input.ContentType) {
		return nil, ErrUnsupportedContent
	}

	imgData, err := io.ReadAll(io.LimitReader(input.Content, maxImageInputBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to read image data: %w", err)
	}

	img, _, err := decodeImage(imgData)
	if err != nil {
		return nil, fmt.Errorf("failed to decode image: %w", err)
	}

	var outputBuf bytes.Buffer
	var outputContentType string

	switch p.Format {
	case "jpeg", "jpg":
		if err := jpeg.Encode(&outputBuf, img, &jpeg.Options{Quality: p.Quality}); err != nil {
			return nil, fmt.Errorf("failed to encode jpeg: %w", err)
		}
		outputContentType = "image/jpeg"
	case "png":
		if err := png.Encode(&outputBuf, img); err != nil {
			return nil, fmt.Errorf("failed to encode png: %w", err)
		}
		outputContentType = "image/png"
	case "webp":
		if err := encodeWebP(&outputBuf, img, p.Quality); err != nil {
			if err := jpeg.Encode(&outputBuf, img, &jpeg.Options{Quality: p.Quality}); err != nil {
				return nil, fmt.Errorf("failed to encode fallback jpeg: %w", err)
			}
			outputContentType = "image/jpeg"
		} else {
			outputContentType = "image/webp"
		}
	default:
		if err := jpeg.Encode(&outputBuf, img, &jpeg.Options{Quality: p.Quality}); err != nil {
			return nil, fmt.Errorf("failed to encode jpeg: %w", err)
		}
		outputContentType = "image/jpeg"
	}

	return &ProcessResult{
		Outputs: []*ObjectOutput{
			{
				Key:         input.Key,
				Content:     &outputBuf,
				Size:        int64(outputBuf.Len()),
				ContentType: outputContentType,
			},
		},
		UpdatedMetadata: map[string]string{
			"compression":     p.Format,
			"quality":         fmt.Sprintf("%d", p.Quality),
			"original_size":   fmt.Sprintf("%d", len(imgData)),
			"compressed_size": fmt.Sprintf("%d", outputBuf.Len()),
		},
	}, nil
}

func encodeWebP(w *bytes.Buffer, img image.Image, quality int) error {
	return fmt.Errorf("webp encoding requires github.com/chai2010/webp library")
}

func (p *RealImageCompressPlugin) CanStream() bool { return false }

func (p *RealImageCompressPlugin) SupportedTypes() []string {
	return []string{"image/jpeg", "image/png", "image/gif", "image/webp"}
}

type RealImageResizePlugin struct {
	Width          int
	Height         int
	MaintainAspect bool
	Algorithm      draw.Scaler
}

func NewRealImageResizePlugin(width, height int, maintainAspect bool) *RealImageResizePlugin {
	return &RealImageResizePlugin{
		Width:          width,
		Height:         height,
		MaintainAspect: maintainAspect,
		Algorithm:      draw.CatmullRom,
	}
}

func (p *RealImageResizePlugin) Name() string { return "image_resize" }

func (p *RealImageResizePlugin) Process(ctx context.Context, input *ObjectInput) (*ProcessResult, error) {
	if !isImageType(input.ContentType) {
		return nil, ErrUnsupportedContent
	}

	imgData, err := io.ReadAll(io.LimitReader(input.Content, maxImageInputBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to read image data: %w", err)
	}

	img, format, err := decodeImage(imgData)
	if err != nil {
		return nil, fmt.Errorf("failed to decode image: %w", err)
	}

	bounds := img.Bounds()
	origWidth := bounds.Dx()
	origHeight := bounds.Dy()

	newWidth := p.Width
	newHeight := p.Height

	if p.MaintainAspect {
		aspectRatio := float64(origWidth) / float64(origHeight)
		if newWidth == 0 && newHeight > 0 {
			newWidth = int(float64(newHeight) * aspectRatio)
		} else if newHeight == 0 && newWidth > 0 {
			newHeight = int(float64(newWidth) / aspectRatio)
		} else if newWidth > 0 && newHeight > 0 {
			targetRatio := float64(newWidth) / float64(newHeight)
			if aspectRatio > targetRatio {
				newHeight = int(float64(newWidth) / aspectRatio)
			} else {
				newWidth = int(float64(newHeight) * aspectRatio)
			}
		}
	}

	if newWidth <= 0 {
		newWidth = origWidth
	}
	if newHeight <= 0 {
		newHeight = origHeight
	}

	dst := image.NewRGBA(image.Rect(0, 0, newWidth, newHeight))
	p.Algorithm.Scale(dst, dst.Bounds(), img, bounds, draw.Over, nil)

	var outputBuf bytes.Buffer
	outputContentType := "image/jpeg"

	switch format {
	case "png":
		if err := png.Encode(&outputBuf, dst); err != nil {
			return nil, fmt.Errorf("failed to encode png: %w", err)
		}
		outputContentType = "image/png"
	case "gif":
		if err := gif.Encode(&outputBuf, dst, nil); err != nil {
			return nil, fmt.Errorf("failed to encode gif: %w", err)
		}
		outputContentType = "image/gif"
	default:
		if err := jpeg.Encode(&outputBuf, dst, &jpeg.Options{Quality: 90}); err != nil {
			return nil, fmt.Errorf("failed to encode jpeg: %w", err)
		}
		outputContentType = "image/jpeg"
	}

	return &ProcessResult{
		Outputs: []*ObjectOutput{
			{
				Key:         input.Key,
				Content:     &outputBuf,
				Size:        int64(outputBuf.Len()),
				ContentType: outputContentType,
			},
		},
		UpdatedMetadata: map[string]string{
			"original_width":  fmt.Sprintf("%d", origWidth),
			"original_height": fmt.Sprintf("%d", origHeight),
			"resized_width":   fmt.Sprintf("%d", newWidth),
			"resized_height":  fmt.Sprintf("%d", newHeight),
		},
	}, nil
}

func (p *RealImageResizePlugin) CanStream() bool { return false }

func (p *RealImageResizePlugin) SupportedTypes() []string {
	return []string{"image/jpeg", "image/png", "image/gif", "image/webp"}
}

type ThumbnailGeneratorPlugin struct {
	Sizes []ThumbnailSize
}

type ThumbnailSize struct {
	Name   string
	Width  int
	Height int
}

func NewThumbnailGeneratorPlugin(sizes []ThumbnailSize) *ThumbnailGeneratorPlugin {
	if len(sizes) == 0 {
		sizes = []ThumbnailSize{
			{Name: "small", Width: 100, Height: 100},
			{Name: "medium", Width: 300, Height: 300},
			{Name: "large", Width: 800, Height: 800},
		}
	}
	return &ThumbnailGeneratorPlugin{Sizes: sizes}
}

func (p *ThumbnailGeneratorPlugin) Name() string { return "thumbnail_generator" }

func (p *ThumbnailGeneratorPlugin) Process(ctx context.Context, input *ObjectInput) (*ProcessResult, error) {
	if !isImageType(input.ContentType) {
		return nil, ErrUnsupportedContent
	}

	imgData, err := io.ReadAll(io.LimitReader(input.Content, maxImageInputBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to read image data: %w", err)
	}

	img, _, err := decodeImage(imgData)
	if err != nil {
		return nil, fmt.Errorf("failed to decode image: %w", err)
	}

	bounds := img.Bounds()
	origWidth := bounds.Dx()
	origHeight := bounds.Dy()

	var outputs []*ObjectOutput
	metadata := make(map[string]string)

	for _, size := range p.Sizes {
		thumbImg := p.resizeToThumbnail(img, origWidth, origHeight, size.Width, size.Height)

		var outputBuf bytes.Buffer
		if err := jpeg.Encode(&outputBuf, thumbImg, &jpeg.Options{Quality: 85}); err != nil {
			continue
		}

		thumbKey := fmt.Sprintf("%s_thumb_%s", input.Key, size.Name)
		outputs = append(outputs, &ObjectOutput{
			Key:         thumbKey,
			Content:     &outputBuf,
			Size:        int64(outputBuf.Len()),
			ContentType: "image/jpeg",
		})

		metadata[fmt.Sprintf("thumbnail_%s", size.Name)] = thumbKey
	}

	return &ProcessResult{
		Outputs:         outputs,
		UpdatedMetadata: metadata,
	}, nil
}

func (p *ThumbnailGeneratorPlugin) resizeToThumbnail(img image.Image, origW, origH, targetW, targetH int) *image.RGBA {
	aspectRatio := float64(origW) / float64(origH)
	targetRatio := float64(targetW) / float64(targetH)

	var newW, newH int
	if aspectRatio > targetRatio {
		newW = targetW
		newH = int(float64(targetW) / aspectRatio)
	} else {
		newH = targetH
		newW = int(float64(targetH) * aspectRatio)
	}

	dst := image.NewRGBA(image.Rect(0, 0, newW, newH))
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, img.Bounds(), draw.Over, nil)

	return dst
}

func (p *ThumbnailGeneratorPlugin) CanStream() bool { return false }

func (p *ThumbnailGeneratorPlugin) SupportedTypes() []string {
	return []string{"image/jpeg", "image/png", "image/gif", "image/webp"}
}

type ImageMetadataExtractorPlugin struct{}

func NewImageMetadataExtractorPlugin() *ImageMetadataExtractorPlugin {
	return &ImageMetadataExtractorPlugin{}
}

func (p *ImageMetadataExtractorPlugin) Name() string { return "image_metadata_extract" }

func (p *ImageMetadataExtractorPlugin) Process(ctx context.Context, input *ObjectInput) (*ProcessResult, error) {
	if !isImageType(input.ContentType) {
		return nil, ErrUnsupportedContent
	}

	imgData, err := io.ReadAll(io.LimitReader(input.Content, maxImageInputBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to read image data: %w", err)
	}

	img, format, err := decodeImage(imgData)
	if err != nil {
		return nil, fmt.Errorf("failed to decode image: %w", err)
	}

	bounds := img.Bounds()
	metadata := map[string]string{
		"width":       fmt.Sprintf("%d", bounds.Dx()),
		"height":      fmt.Sprintf("%d", bounds.Dy()),
		"format":      format,
		"size_bytes":  fmt.Sprintf("%d", len(imgData)),
		"color_model": fmt.Sprintf("%v", img.ColorModel()),
		"has_alpha":   fmt.Sprintf("%v", hasAlpha(img)),
	}

	return &ProcessResult{
		UpdatedMetadata: metadata,
	}, nil
}

func (p *ImageMetadataExtractorPlugin) CanStream() bool { return false }

func (p *ImageMetadataExtractorPlugin) SupportedTypes() []string {
	return []string{"image/jpeg", "image/png", "image/gif", "image/webp"}
}

func isImageType(contentType string) bool {
	return contentType == "image/jpeg" ||
		contentType == "image/png" ||
		contentType == "image/gif" ||
		contentType == "image/webp" ||
		contentType == "image/bmp" ||
		contentType == "image/tiff"
}

func decodeImage(data []byte) (image.Image, string, error) {
	reader := bytes.NewReader(data)

	img, format, err := image.Decode(reader)
	if err == nil {
		if err := validateImageBounds(img); err != nil {
			return nil, "", err
		}
		return img, format, nil
	}

	reader.Seek(0, 0)
	if webpImg, err := webp.Decode(reader); err == nil {
		if err := validateImageBounds(webpImg); err != nil {
			return nil, "", err
		}
		return webpImg, "webp", nil
	}

	return nil, "", fmt.Errorf("unsupported image format")
}

func validateImageBounds(img image.Image) error {
	bounds := img.Bounds()
	pixels := int64(bounds.Dx()) * int64(bounds.Dy())
	if pixels > maxImagePixels {
		return fmt.Errorf("image dimensions %dx%d exceed maximum allowed pixels", bounds.Dx(), bounds.Dy())
	}
	return nil
}

func hasAlpha(img image.Image) bool {
	switch img.(type) {
	case *image.RGBA, *image.NRGBA, *image.Paletted:
		return true
	default:
		return false
	}
}

func RegisterRealImagePlugins(e *PipelineExecutor, quality int) error {
	plugins := []PipelinePlugin{
		NewRealImageCompressPlugin(quality, "jpeg"),
		NewRealImageResizePlugin(0, 0, true),
		NewThumbnailGeneratorPlugin(nil),
		NewImageMetadataExtractorPlugin(),
	}

	for _, plugin := range plugins {
		if err := e.RegisterPlugin(plugin); err != nil {
			return err
		}
	}

	return nil
}
