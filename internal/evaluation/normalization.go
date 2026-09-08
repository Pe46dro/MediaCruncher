package evaluation

import (
	"fmt"
	"strings"
)

// NormalizedMetadata is the rule-engine-compatible representation of analysis data.
type NormalizedMetadata struct {
	VideoCodec        string   `json:"video_codec"`
	AudioCodec        string   `json:"audio_codec"`
	Resolution        string   `json:"resolution"`
	Width             int      `json:"width"`
	Height            int      `json:"height"`
	FrameRate         float64  `json:"frame_rate"`
	BitRate           int64    `json:"bit_rate"`
	Duration          float64  `json:"duration"`
	ColorSpace        string   `json:"color_space"`
	IsHDR             bool     `json:"is_hdr"`
	HighBitDepth      bool     `json:"high_bit_depth"`
	ContainerFormat   string   `json:"container_format"`
	StreamCount       int      `json:"stream_count"`
	Languages         []string `json:"languages"`
	HasSubtitles      bool     `json:"has_subtitles"`
	EstimatedOutput   int64    `json:"estimated_output_size"`
	AnomalyFlags      []string `json:"anomaly_flags"`
	OriginalFilePath  string   `json:"original_file_path"`
}

// Normalize transforms an AnalysisResult into NormalizedMetadata with standard units.
func Normalize(result *AnalysisResult) *NormalizedMetadata {
	if result == nil {
		return &NormalizedMetadata{
			AnomalyFlags: []string{"nil_analysis"},
		}
	}

	nm := &NormalizedMetadata{
		VideoCodec:      normalizeCodec(result.VideoCodec),
		AudioCodec:      normalizeCodec(result.AudioCodec),
		Resolution:      result.Resolution,
		Width:           result.Width,
		Height:          result.Height,
		FrameRate:       result.FrameRate,
		BitRate:         result.BitRate,
		Duration:        result.Duration,
		ColorSpace:      strings.ToLower(result.ColorSpace),
		ContainerFormat: strings.ToLower(result.ContainerFormat),
		StreamCount:     result.StreamCount,
		Languages:       result.Languages,
		HasSubtitles:    result.HasSubtitles,
		OriginalFilePath: result.FilePath,
	}

	nm.IsHDR = detectHDR(result)
	nm.HighBitDepth = detectHighBitDepth(result)
	nm.EstimatedOutput = estimateOutputSize(result)
	nm.AnomalyFlags = append(nm.AnomalyFlags, result.AnomalyFlags...)

	if result.Error != "" {
		nm.AnomalyFlags = append(nm.AnomalyFlags, fmt.Sprintf("probe_error: %s", result.Error))
	}

	if result.IsPartial {
		nm.AnomalyFlags = append(nm.AnomalyFlags, "partial_analysis")
	}

	return nm
}

// normalizeCodec resolves codec aliases to standard names.
func normalizeCodec(codec string) string {
	if codec == "" {
		return "unknown"
	}
	c := strings.ToLower(codec)
	switch c {
	case "h264", "avc":
		return "h.264"
	case "hevc", "h265", "h.265":
		return "h.265"
	case "vp9":
		return "vp9"
	case "av1":
		return "av1"
	case "mpeg4", "mp4v":
		return "mpeg-4"
	case "mpeg2video":
		return "mpeg-2"
	case "aac":
		return "aac"
	case "mp3":
		return "mp3"
	case "flac":
		return "flac"
	case "opus":
		return "opus"
	case "vorbis":
		return "vorbis"
	default:
		return c
	}
}

// detectHDR checks if the analysis indicates HDR content.
func detectHDR(result *AnalysisResult) bool {
	if result.BitsPerChannel >= 10 {
		return true
	}
	switch strings.ToLower(result.ColorSpace) {
	case "bt2020", "rgb", "bt2020nc", "bt2020c":
		return true
	}
	if result.BitsPerChannel == 0 {
		for _, flag := range result.AnomalyFlags {
			if flag == "hdr_possible" {
				return true
			}
		}
	}
	return false
}

// detectHighBitDepth checks if the content uses 10-bit or deeper encoding.
func detectHighBitDepth(result *AnalysisResult) bool {
	if result.BitsPerChannel >= 10 {
		return true
	}
	return false
}

// estimateOutputSize calculates estimated output size at target bitrate (5 Mbps for video).
func estimateOutputSize(result *AnalysisResult) int64 {
	if result.Duration <= 0 || result.BitRate <= 0 {
		return 0
	}
	targetBitrate := int64(5000000)
	estimatedBytes := int64(result.Duration) * targetBitrate / 8
	return estimatedBytes
}
