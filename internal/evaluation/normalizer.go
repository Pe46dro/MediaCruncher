package evaluation

import (
	"os"
	"strings"
)

type NormalizedMetadata struct {
	FilePath       string          `json:"file_path"`
	Container      string          `json:"container"`
	Duration       float64         `json:"duration"`
	TotalBitrate   int64           `json:"total_bitrate"`
	FileSize       int64           `json:"file_size"`
	VideoCodec     string          `json:"video_codec"`
	Width          int             `json:"width"`
	Height         int             `json:"height"`
	ResolutionTag  string          `json:"resolution_tag"` // 4k, 1080p, 720p, 480p, sd
	FrameRate      float64         `json:"frame_rate"`
	BitDepth       int             `json:"bit_depth"`
	IsHDR          bool            `json:"is_hdr"`
	HDRFormat      string          `json:"hdr_format,omitempty"` // hdr10, dolby_vision, hlg
	AudioTracks    []AudioTrack    `json:"audio_tracks"`
	SubtitleTracks []SubtitleTrack `json:"subtitle_tracks"`
	HasSurround    bool            `json:"has_surround"`
	HasLossless    bool            `json:"has_lossless"`
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
