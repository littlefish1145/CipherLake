package pipeline

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"
)

var piiPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\b\d{15}\b`),
	regexp.MustCompile(`\b\d{18}\b`),
	regexp.MustCompile(`\b1[3-9]\d{9}\b`),
	regexp.MustCompile(`\b\d{4}[-\s]?\d{4}[-\s]?\d{4}[-\s]?\d{4}\b`),
}

type ImageCompressPlugin struct{}

func (p *ImageCompressPlugin) Name() string { return "image_compress" }

func (p *ImageCompressPlugin) Process(ctx context.Context, input *ObjectInput) (*ProcessResult, error) {
	if !strings.HasPrefix(input.ContentType, "image/") {
		return nil, ErrUnsupportedContent
	}
	return &ProcessResult{
		UpdatedMetadata: map[string]string{"compression": "webp", "quality": "80"},
	}, nil
}

func (p *ImageCompressPlugin) CanStream() bool          { return true }
func (p *ImageCompressPlugin) SupportedTypes() []string  { return []string{"image/jpeg", "image/png", "image/gif", "image/webp"} }

type ImageResizePlugin struct{}

func (p *ImageResizePlugin) Name() string { return "image_resize" }

func (p *ImageResizePlugin) Process(ctx context.Context, input *ObjectInput) (*ProcessResult, error) {
	if !strings.HasPrefix(input.ContentType, "image/") {
		return nil, ErrUnsupportedContent
	}
	return &ProcessResult{
		UpdatedMetadata: map[string]string{"resized": "true"},
	}, nil
}

func (p *ImageResizePlugin) CanStream() bool          { return true }
func (p *ImageResizePlugin) SupportedTypes() []string  { return []string{"image/jpeg", "image/png", "image/gif", "image/webp"} }

type MetadataExtractPlugin struct{}

func (p *MetadataExtractPlugin) Name() string { return "metadata_extract" }

func (p *MetadataExtractPlugin) Process(ctx context.Context, input *ObjectInput) (*ProcessResult, error) {
	md := make(map[string]string)
	switch {
	case strings.HasPrefix(input.ContentType, "image/"):
		md["extracted_from"] = "image"
	case strings.HasPrefix(input.ContentType, "video/"):
		md["extracted_from"] = "video"
	case strings.HasPrefix(input.ContentType, "audio/"):
		md["extracted_from"] = "audio"
	case input.ContentType == "application/pdf":
		md["extracted_from"] = "pdf"
	}
	return &ProcessResult{UpdatedMetadata: md}, nil
}

func (p *MetadataExtractPlugin) CanStream() bool          { return false }
func (p *MetadataExtractPlugin) SupportedTypes() []string  { return []string{"image/*", "video/*", "audio/*", "application/pdf"} }

type EncryptPIIPlugin struct{}

func (p *EncryptPIIPlugin) Name() string { return "encrypt_pii" }

func (p *EncryptPIIPlugin) Process(ctx context.Context, input *ObjectInput) (*ProcessResult, error) {
	content, err := io.ReadAll(io.LimitReader(input.Content, 50*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("failed to read content: %w", err)
	}
	contentStr := string(content)
	redacted := contentStr
	for _, pattern := range piiPatterns {
		redacted = pattern.ReplaceAllStringFunc(redacted, func(match string) string { return maskString(match) })
	}
	result := &ProcessResult{UpdatedMetadata: map[string]string{"pii_redacted": "true"}}
	if redacted != contentStr {
		result.Outputs = []*ObjectOutput{{
			Key: input.Key + ".redacted", Content: strings.NewReader(redacted),
			Size: int64(len(redacted)), ContentType: input.ContentType,
		}}
	}
	return result, nil
}

func maskString(s string) string {
	if len(s) <= 4 {
		return "****"
	}
	return s[:2] + strings.Repeat("*", len(s)-4) + s[len(s)-2:]
}

func (p *EncryptPIIPlugin) CanStream() bool          { return false }
func (p *EncryptPIIPlugin) SupportedTypes() []string  { return []string{"text/*", "application/json", "application/xml"} }

type VideoThumbnailPlugin struct{}

func (p *VideoThumbnailPlugin) Name() string { return "video_thumbnail" }

func (p *VideoThumbnailPlugin) Process(ctx context.Context, input *ObjectInput) (*ProcessResult, error) {
	if !strings.HasPrefix(input.ContentType, "video/") {
		return nil, ErrUnsupportedContent
	}
	return &ProcessResult{UpdatedMetadata: map[string]string{"thumbnail_generated": "true"}}, nil
}

func (p *VideoThumbnailPlugin) CanStream() bool          { return false }
func (p *VideoThumbnailPlugin) SupportedTypes() []string  { return []string{"video/*"} }

type PDFToTextPlugin struct{}

func (p *PDFToTextPlugin) Name() string { return "pdf_to_text" }

func (p *PDFToTextPlugin) Process(ctx context.Context, input *ObjectInput) (*ProcessResult, error) {
	if input.ContentType != "application/pdf" {
		return nil, ErrUnsupportedContent
	}
	return &ProcessResult{UpdatedMetadata: map[string]string{"pdf_converted": "true"}}, nil
}

func (p *PDFToTextPlugin) CanStream() bool          { return false }
func (p *PDFToTextPlugin) SupportedTypes() []string  { return []string{"application/pdf"} }
