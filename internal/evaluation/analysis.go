package evaluation

import (
	"fmt"
	"time"
)

// AnalysisResult represents the output of the ffprobe analysis stage.
type AnalysisResult struct {
	FilePath        string                 `json:"file_path"`
	VideoCodec      string                 `json:"video_codec"`
	AudioCodec      string                 `json:"audio_codec"`
	Resolution      string                 `json:"resolution"`
	Width           int                    `json:"width"`
	Height          int                    `json:"height"`
	FrameRate       float64                `json:"frame_rate"`
	BitRate         int64                  `json:"bit_rate"`
	Duration        float64                `json:"duration"`
	ColorSpace      string                 `json:"color_space"`
	BitsPerChannel  int                    `json:"bits_per_channel"`
	ContainerFormat string                 `json:"container_format"`
	StreamCount     int                    `json:"stream_count"`
	Languages       []string               `json:"languages"`
	HasSubtitles    bool                   `json:"has_subtitles"`
	AnomalyFlags    []string               `json:"anomaly_flags"`
	ProbeResult     *ProbeResult           `json:"probe_result"`
	ProbeTime       time.Duration          `json:"probe_time"`
	Error           string                 `json:"error"`
	IsPartial       bool                   `json:"is_partial"`
	Timestamp       time.Time              `json:"timestamp"`
	RawProbeData    map[string]interface{} `json:"raw_probe_data,omitempty"`
}

// BuildAnalysisResult constructs an AnalysisResult from a ProbeResult.
func BuildAnalysisResult(filePath string, probe *ProbeResult, rawData map[string]interface{}) *AnalysisResult {
	result := &AnalysisResult{
		FilePath:   filePath,
		ProbeResult: probe,
		ProbeTime:  probe.ProbeTime,
		Timestamp:  time.Now(),
		Error:      probe.Error,
		IsPartial:  probe.IsPartial,
	}

	if rawData != nil {
		result.RawProbeData = rawData
	}

	if probe == nil {
		result.AnomalyFlags = append(result.AnomalyFlags, "no_probe_data")
		return result
	}

	if probe.ParseError != "" {
		result.AnomalyFlags = append(result.AnomalyFlags, "parse_error")
	}

	if probe.Error != "" && probe.ExitCode != 0 {
		result.AnomalyFlags = append(result.AnomalyFlags, "probe_error")
	}

	for _, stream := range probe.Streams {
		switch stream.Type {
		case "video":
			if result.VideoCodec == "" {
				result.VideoCodec = stream.CodeciName
				result.Width = stream.Width
				result.Height = stream.Height
				result.FrameRate = DecodeStreamRate(stream.Rate)
				result.BitRate += DecodeBitRate(stream.BitRate)
				result.ColorSpace = stream.ColorSpace
				if stream.ColorPrimaries != "" {
					result.BitsPerChannel = stream.BitsPerChannel
				}
				if stream.Height >= 2160 || (stream.ColorSpace == "bt2020" || stream.ColorSpace == "rgb") {
					result.AnomalyFlags = append(result.AnomalyFlags, "hdr_possible")
				}
			}
		case "audio":
			if result.AudioCodec == "" {
				result.AudioCodec = stream.CodeciName
				result.BitRate += DecodeBitRate(stream.BitRate)
				language := stream.ProbeTags["language"]
				if language != "" && language != "und" {
					result.Languages = append(result.Languages, language)
				}
			}
		}
	}

	for _, stream := range probe.Streams {
		if stream.Type == "subtitle" {
			result.HasSubtitles = true
		}
		if stream.Disposition.Default != 0 && stream.Type == "subtitle" {
			result.AnomalyFlags = append(result.AnomalyFlags, "embedded_default_subtitle")
		}
	}

	result.StreamCount = len(probe.Streams)

	if probe.Format.Streams > 0 {
		result.ContainerFormat = probe.Format.Name
		if probe.Format.Duration > 0 {
			result.Duration = probe.Format.Duration
		} else if len(probe.Streams) > 0 {
			result.Duration = probe.Streams[0].Duration
		}
	}

	if probe.Duration > 0 && result.Duration == 0 {
		result.Duration = probe.Duration
	}

	if result.Width > 0 && result.Height > 0 {
		result.Resolution = fmt.Sprintf("%dx%d", result.Width, result.Height)
	}

	if len(result.Languages) == 0 {
		result.Languages = nil
	}

	return result
}
