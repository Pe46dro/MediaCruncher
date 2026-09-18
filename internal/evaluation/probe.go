package evaluation

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"mediacruncher/internal/proc"
)

type ProbeOutput struct {
	Streams  []ProbeStream  `json:"streams"`
	Format   ProbeFormat    `json:"format"`
	Chapters []ProbeChapter `json:"chapters"`
}

type ProbeStream struct {
	Index              int               `json:"index"`
	CodecName          string            `json:"codec_name"`
	CodecLongName      string            `json:"codec_long_name"`
	CodecType          string            `json:"codec_type"` // video, audio, subtitle
	Profile            string            `json:"profile"`
	Width              int               `json:"width"`
	Height             int               `json:"height"`
	RFrameRate         string            `json:"r_frame_rate"`
	AvgFrameRate       string            `json:"avg_frame_rate"`
	PixFmt             string            `json:"pix_fmt"`
	ColorRange         string            `json:"color_range"`
	ColorSpace         string            `json:"color_space"`
	ColorTransfer      string            `json:"color_transfer"`
	ColorPrimaries     string            `json:"color_primaries"`
	BitRate            string            `json:"bit_rate"`
	Channels           int               `json:"channels"`
	ChannelLayout      string            `json:"channel_layout"`
	SampleRate         string            `json:"sample_rate"`
	Tags               map[string]string `json:"tags"`
	Disposition        map[string]int    `json:"disposition"`
	SideDataList       []map[string]any  `json:"side_data_list"`
}

type ProbeFormat struct {
	Filename       string            `json:"filename"`
	NbStreams      int               `json:"nb_streams"`
	FormatName     string            `json:"format_name"`
	FormatLongName string            `json:"format_long_name"`
	Duration       string            `json:"duration"`
	Size           string            `json:"size"`
	BitRate        string            `json:"bit_rate"`
	Tags           map[string]string `json:"tags"`
}

type ProbeChapter struct {
	ID        int     `json:"id"`
	TimeBase  string  `json:"time_base"`
	Start     int64   `json:"start"`
	StartTime string  `json:"start_time"`
	End       int64   `json:"end"`
	EndTime   string  `json:"end_time"`
	Tags      map[string]string `json:"tags"`
}

// ProbeFile executes ffprobe and parses the JSON output into ProbeOutput.
func ProbeFile(ctx context.Context, filePath string, timeout time.Duration) (*ProbeOutput, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{
		"-v", "quiet",
		"-print_format", "json",
		"-show_format",
		"-show_streams",
		"-show_chapters",
		filePath,
	}

	stdout, stderr, err := proc.RunCommand(probeCtx, "ffprobe", args...)
	if err != nil {
		return nil, fmt.Errorf("ffprobe failed on %s: %w (stderr: %s)", filePath, err, string(stderr))
	}

	var output ProbeOutput
	if err := json.Unmarshal(stdout, &output); err != nil {
		return nil, fmt.Errorf("failed to parse ffprobe json: %w", err)
	}

	return &output, nil
}

// ParseBitrate parses a string bitrate into an int64.
func ParseBitrate(val string) int64 {
	val = strings.TrimSpace(val)
	if val == "" || val == "N/A" {
		return 0
	}
	n, _ := strconv.ParseInt(val, 10, 64)
	return n
}

// ParseFloat parses a string float into float64.
func ParseFloat(val string) float64 {
	val = strings.TrimSpace(val)
	if val == "" || val == "N/A" {
		return 0
	}
	f, _ := strconv.ParseFloat(val, 64)
	return f
}
