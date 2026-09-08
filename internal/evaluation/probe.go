package evaluation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"
)

// ProbeConfig holds configuration for ffprobe invocations.
type ProbeConfig struct {
	Timeout    time.Duration
	BinaryPath string
}

// ProbeResult contains the raw ffprobe output parsed into structured data.
type ProbeResult struct {
	Format     ProbeFormat     `json:"format"`
	Streams    []ProbeStream   `json:"streams"`
	Error      string          `json:"error"`
	ExitCode   int             `json:"exit_code"`
	Duration   float64         `json:"duration"`
	ProbeTime  time.Duration   `json:"probe_time"`
	ParseError string          `json:"parse_error"`
	IsPartial  bool            `json:"is_partial"`
}

// ProbeFormat represents the container-level metadata from ffprobe.
type ProbeFormat struct {
	Name            string            `json:"format_name"`
	LongName        string            `json:"format_long_name"`
	StartTime       float64           `json:"start_time"`
	Duration        float64           `json:"duration"`
	Size            string            `json:"size"`
	BitRate         string            `json:"bit_rate"`
	ProbeName       string            `json:"probe_name"`
	Streams         int               `json:"nb_streams"`
	ProbeTags       map[string]string `json:"tags"`
}

// ProbeStream represents a single stream's metadata from ffprobe.
type ProbeStream struct {
	Index          int               `json:"index"`
	Codecid        string            `json:"codec_id"`
	CodeciName     string            `json:"codec_name"`
	CodecLongName  string            `json:"codec_long_name"`
	Type           string            `json:"codec_type"`
	Width          int               `json:"width"`
	Height         int               `json:"height"`
	FrameCount     string            `json:"nb_frames"`
	Rate           string            `json:"r_frame_rate"`
	AvgRate        string            `json:"avg_frame_rate"`
	BitRate        string            `json:"bit_rate"`
	StartTime      float64           `json:"start_time"`
	Duration       float64           `json:"duration"`
	SampleFmt      string            `json:"sample_fmt"`
	SampleRate     string            `json:"sample_rate"`
	Channels       int               `json:"channels"`
	ChannelsLayout string            `json:"channel_layout"`
	ProbeTags      map[string]string `json:"tags"`
	Disposition    ProbeDisposition  `json:"disposition"`
	ColorPrimaries string            `json:"color_primaries"`
	ColorSpace     string            `json:"color_space"`
	ColorTransfer  string            `json:"color_transfer"`
	BitsPerChannel int               `json:"bits_per_channel"`
}

// ProbeDisposition represents stream disposition flags.
type ProbeDisposition struct {
	Default      int `json:"default"`
	Dub          int `json:"dub"`
	Original     int `json:"original"`
	Comment      int `json:"comment"`
	Lyrics       int `json:"lyrics"`
	Karaoke      int `json:"karaoke"`
	Provided     int `json:"provided"`
	Associated   int `json:"associated"`
	Visuals      int `json:"visuals"`
	Imagery      int `json:"imagery"`
	Captions     int `json:"captions"`
	Descriptions int `json:"descriptions"`
	Metadata     int `json:"metadata"`
}

// Probe invokes ffprobe on the given file path and returns the parsed result.
func Probe(ctx context.Context, path string, cfg ProbeConfig) *ProbeResult {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.BinaryPath == "" {
		cfg.BinaryPath = "ffprobe"
	}

	start := time.Now()
	args := []string{
		"-v", "quiet",
		"-print_format", "json",
		"-show_format",
		"-show_streams",
		path,
	}

	cmd := exec.CommandContext(ctx, cfg.BinaryPath, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	probeTime := time.Since(start)

	result := &ProbeResult{
		ProbeTime: probeTime,
	}

	if ctx.Err() != nil {
		result.Error = fmt.Sprintf("context cancelled: %v", ctx.Err())
		result.ExitCode = -1
		return result
	}

	if err != nil {
		result.Error = err.Error()
		result.ExitCode = 1
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
		}
		return result
	}

	result.ExitCode = 0
	err = json.Unmarshal(stdout.Bytes(), result)
	if err != nil {
		result.ParseError = err.Error()
		result.IsPartial = true
		result.Error = stderr.String()
		return result
	}

	if result.Format.Name == "" && len(result.Streams) == 0 {
		result.ParseError = "empty probe output"
		result.IsPartial = true
	}

	return result
}

// DecodeStreamRate converts an ffprobe rate string like "30000/1001" to float64 fps.
func DecodeStreamRate(rate string) float64 {
	var numerator, denominator int
	n, err := fmt.Sscanf(rate, "%d/%d", &numerator, &denominator)
	if n == 2 && denominator > 0 && err == nil {
		return float64(numerator) / float64(denominator)
	}
	f, err := fmt.Sscanf(rate, "%f", &numerator)
	if f == 1 && err == nil {
		return float64(numerator)
	}
	return 0
}

// DecodeBitRate converts a ffprobe bit rate string to bits per second as int64.
func DecodeBitRate(bitRate string) int64 {
	var rate int64
	_, err := fmt.Sscanf(bitRate, "%d", &rate)
	if err == nil {
		return rate
	}
	if len(bitRate) >= 2 {
		suffix := bitRate[len(bitRate)-2:]
		var base int64
		_, err = fmt.Sscanf(bitRate[:len(bitRate)-2], "%d", &base)
		if err != nil {
			return 0
		}
		switch suffix {
		case "k":
			return base * 1000
		case "m":
			return base * 1000000
		case "g":
			return base * 1000000000
		}
	}
	return 0
}

// DecodeDuration converts a ffprobe duration string to float64 seconds.
func DecodeDuration(duration string) float64 {
	var d float64
	fmt.Sscanf(duration, "%f", &d)
	return d
}
