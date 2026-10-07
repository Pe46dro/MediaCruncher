package evaluation

import (
	"fmt"
	"os"
	"strings"
)

type NormalizedMetadata struct {
	FilePath               string          `json:"file_path"`
	Container              string          `json:"container"`
	Duration               float64         `json:"duration"`
	TotalBitrate           int64           `json:"total_bitrate"`
	FileSize               int64           `json:"file_size"`
	VideoCodec             string          `json:"video_codec"`
	Width                  int             `json:"width"`
	Height                 int             `json:"height"`
	ResolutionTag          string          `json:"resolution_tag"` // 4k, 1080p, 720p, 480p, sd
	FrameRate              float64         `json:"frame_rate"`
	BitDepth               int             `json:"bit_depth"`
	PixFmt                 string          `json:"pix_fmt"`             // raw pixel format from probe (yuv420p, yuv444p10le, etc.)
	ChromaSubsampling      string          `json:"chroma_subsampling"`  // 4:0:0, 4:1:0, 4:2:0, 4:2:2, 4:4:4
	NeedsManualReview      bool            `json:"needs_manual_review"` // true if pix_fmt needs manual transcoding decision
	PixFmtReviewReason     string          `json:"pix_fmt_review_reason,omitempty"`
	IsHDR                  bool            `json:"is_hdr"`
	HDRFormat              string          `json:"hdr_format,omitempty"` // hdr10, dolby_vision, hlg
	AudioTracks            []AudioTrack    `json:"audio_tracks"`
	SubtitleTracks         []SubtitleTrack `json:"subtitle_tracks"`
	AttachedPicIndices     []int           `json:"attached_pic_indices"` // stream indices of attached picture/video covers
	HasSurround            bool            `json:"has_surround"`
	HasLossless            bool            `json:"has_lossless"`
}

type AudioTrack struct {
	Index       int    `json:"index"`
	Codec       string `json:"codec"`
	Channels    int    `json:"channels"`
	Layout      string `json:"layout"`
	Bitrate     int64  `json:"bitrate"`
	SampleRate  int    `json:"sample_rate"`
	Language    string `json:"language"`
	Title       string `json:"title"`
	IsDefault   bool   `json:"is_default"`
	IsSurround  bool   `json:"is_surround"` // >= 6 channels (5.1, 7.1)
	IsLossless  bool   `json:"is_lossless"` // truehd, dts-hd, flac, pcm
}

type SubtitleTrack struct {
	Index     int    `json:"index"`
	Codec     string `json:"codec"`
	Language  string `json:"language"`
	Title     string `json:"title"`
	IsDefault bool   `json:"is_default"`
	IsForced  bool   `json:"is_forced"`
}

// Normalize transforms raw probe output into rule-engine compatible metadata.
func Normalize(raw *ProbeOutput, filePath string, aliases map[string]string) *NormalizedMetadata {
	norm := &NormalizedMetadata{
		FilePath:     filePath,
		Container:    raw.Format.FormatName,
		Duration:     ParseFloat(raw.Format.Duration),
		TotalBitrate: ParseBitrate(raw.Format.BitRate),
		FileSize:     ParseBitrate(raw.Format.Size),
	}
	if norm.FileSize == 0 {
		if fi, err := os.Stat(filePath); err == nil {
			norm.FileSize = fi.Size()
		}
	}

	for _, st := range raw.Streams {
		switch st.CodecType {
		case "video":
			// Detect attached pictures (covers, posters) - these are image codecs embedded in video streams
			// They cause FFmpeg crashes (SIGSEGV) when transcode attempts to process them
			isAttachedPic := isAttachedPicture(st)
			if isAttachedPic {
				norm.AttachedPicIndices = append(norm.AttachedPicIndices, st.Index)
				break // Skip attached pictures - don't process as regular video
			}
			
			if norm.VideoCodec == "" { // Use primary video stream
				rawCodec := strings.ToLower(st.CodecName)
				if resolved, ok := aliases[rawCodec]; ok {
					norm.VideoCodec = resolved
				} else {
					norm.VideoCodec = rawCodec
				}

				norm.Width = st.Width
				norm.Height = st.Height
				norm.ResolutionTag = classifyResolution(st.Width, st.Height)

				// Determine bit depth & pixel format
				norm.BitDepth = extractBitDepth(st.PixFmt)

				// Pixel format safety check
				norm.PixFmt = st.PixFmt
				norm.ChromaSubsampling = classifyChromaSubsampling(st.PixFmt)
				if needsReview, reason := checkPixFmtSafety(st.PixFmt); needsReview {
					norm.NeedsManualReview = true
					norm.PixFmtReviewReason = reason
				}

				// Determine HDR
				norm.IsHDR, norm.HDRFormat = detectHDR(st)
			}

		case "audio":
			track := AudioTrack{
				Index:      st.Index,
				Codec:      strings.ToLower(st.CodecName),
				Channels:   st.Channels,
				Layout:     st.ChannelLayout,
				Bitrate:    ParseBitrate(st.BitRate),
				Language:   strings.ToLower(st.Tags["language"]),
				Title:      st.Tags["title"],
				IsDefault:  st.Disposition["default"] == 1,
				IsSurround: st.Channels >= 6,
				IsLossless: isLosslessAudio(st.CodecName),
			}
			if track.IsSurround {
				norm.HasSurround = true
			}
			if track.IsLossless {
				norm.HasLossless = true
			}
			norm.AudioTracks = append(norm.AudioTracks, track)

		case "subtitle":
			track := SubtitleTrack{
				Index:     st.Index,
				Codec:     strings.ToLower(st.CodecName),
				Language:  strings.ToLower(st.Tags["language"]),
				Title:     st.Tags["title"],
				IsDefault: st.Disposition["default"] == 1,
				IsForced:  st.Disposition["forced"] == 1,
			}
			norm.SubtitleTracks = append(norm.SubtitleTracks, track)
		}
	}

	return norm
}

// isAttachedPicture detects if a video stream is an attached picture (cover, poster, etc.)
// Attached pictures are image codecs (png, jpeg, bmp) embedded in video streams.
// They cause FFmpeg crashes (SIGSEGV) when transcode attempts to process them.
func isAttachedPicture(st ProbeStream) bool {
	// Image codecs commonly used as attached pictures
	imageCodecs := []string{"png", "jpeg", "jpg", "bmp", "mjpeg"}
	for _, codec := range imageCodecs {
		if strings.EqualFold(st.CodecName, codec) {
			return true
		}
	}
	return false
}

func classifyResolution(width, height int) string {
	if width >= 3800 || height >= 2100 {
		return "4k"
	}
	if width >= 1900 || height >= 1000 {
		return "1080p"
	}
	if width >= 1200 || height >= 700 {
		return "720p"
	}
	if width >= 800 || height >= 450 {
		return "480p"
	}
	return "sd"
}

func extractBitDepth(pixFmt string) int {
	if strings.Contains(pixFmt, "12") {
		return 12
	}
	if strings.Contains(pixFmt, "10") {
		return 10
	}
	return 8
}

func detectHDR(st ProbeStream) (bool, string) {
	// Check transfer characteristics & primaries
	colorTransfer := strings.ToLower(st.ColorTransfer)
	colorPrimaries := strings.ToLower(st.ColorPrimaries)

	if colorTransfer == "smpte2084" || strings.Contains(colorPrimaries, "2020") {
		// Check side data for Dolby Vision
		for _, side := range st.SideDataList {
			if sideType, ok := side["side_data_type"].(string); ok {
				if strings.Contains(strings.ToLower(sideType), "dovi") || strings.Contains(strings.ToLower(sideType), "dolby") {
					return true, "dolby_vision"
				}
			}
		}
		return true, "hdr10"
	}
	if colorTransfer == "arib-std-b67" {
		return true, "hlg"
	}
	return false, ""
}

func isLosslessAudio(codec string) bool {
	c := strings.ToLower(codec)
	return strings.Contains(c, "truehd") ||
		strings.Contains(c, "dts-hd") ||
		strings.Contains(c, "flac") ||
		strings.Contains(c, "pcm") ||
		strings.Contains(c, "alac")
}

// classifyChromaSubsampling determines the chroma subsampling ratio from pixel format string.
// Returns: "4:4:4", "4:2:2", "4:2:0", "4:1:0", "4:0:0"
func classifyChromaSubsampling(pixFmt string) string {
	lower := strings.ToLower(pixFmt)

	// 4:4:4 formats (no subsampling)
	if strings.Contains(lower, "444") {
		return "4:4:4"
	}
	// 4:2:2 formats
	if strings.Contains(lower, "422") {
		return "4:2:2"
	}
	// 4:1:0 formats (rare, typically cmyk)
	if strings.Contains(lower, "410") {
		return "4:1:0"
	}
	// 4:2:0 formats (most common)
	if strings.Contains(lower, "420") {
		return "4:2:0"
	}
	// 4:0:0 (grayscale or other)
	if strings.Contains(lower, "gray") || strings.Contains(lower, "mono") {
		return "4:0:0"
	}
	// Fallback: assume 4:2:0 (standard)
	return "4:2:0"
}

// checkPixFmtSafety evaluates whether a pixel format requires manual review before transcoding.
// Returns (needsReview, reason).
//
// Pixel formats that trigger review:
// - 10-bit and 12-bit formats (loss of quality converting to 8-bit yuv420p)
// - 4:4:4 chroma subsampling (high chroma detail, potential segfault with some decoders)
// - 4:2:2 chroma subsampling (loss of chroma detail)
// - 16-bit formats (typically unused by x265, will fail or crash)
// - Planar formats with alpha channels (yuva444p16le, etc.)
//
// Safe to transcode automatically:
// - 8-bit 4:2:0 (yuv420p)
// - 8-bit 4:2:2 (yuv422p) — minor chroma loss acceptable
func checkPixFmtSafety(pixFmt string) (bool, string) {
	lower := strings.ToLower(pixFmt)

	// 16-bit formats — x265 doesn't support these, will likely segfault
	if strings.Contains(lower, "16") {
		return true, fmt.Sprintf("16-bit pixel format (%s) not supported by x265; manual review required", pixFmt)
	}

	// 10-bit formats — converting to 8-bit yuv420p loses 33% of luminance range
	if strings.Contains(lower, "10") {
		return true, fmt.Sprintf("10-bit pixel format (%s) will lose quality converting to 8-bit yuv420p", pixFmt)
	}

	// 12-bit formats — converting to 8-bit yuv420p loses 50% of luminance range
	if strings.Contains(lower, "12") {
		return true, fmt.Sprintf("12-bit pixel format (%s) will lose significant quality converting to 8-bit yuv420p", pixFmt)
	}

	// 4:4:4 chroma — high chroma detail, more bandwidth, potential segfault with hardware decoders
	if strings.Contains(lower, "444") {
		return true, fmt.Sprintf("4:4:4 chroma subsampling (%s) may cause decoder issues and loses chroma detail converting to 4:2:0", pixFmt)
	}

	// 4:2:2 chroma — minor quality loss, but generally safe
	// Only flag if combined with high bit depth (catches edge cases)
	if strings.Contains(lower, "422") && strings.Contains(lower, "10") {
		return true, fmt.Sprintf("4:2:2 chroma with 10-bit (%s) loses quality converting to 8-bit yuv420p", pixFmt)
	}

	// Safe formats (8-bit 4:2:0, 8-bit 4:2:2, etc.) — no review needed
	return false, ""
}
