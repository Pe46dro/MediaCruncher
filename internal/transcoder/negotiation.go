package transcoder

import (
	"fmt"
	"time"
)

// EncodingPreset is a configuration of encoding parameters derived from the evaluation pipeline.
type EncodingPreset struct {
	TargetCodec        string  `json:"target_codec"`
	QualityLevel       int     `json:"quality_level"`
	Resolution         string  `json:"resolution"`
	Width              int     `json:"width"`
	Height             int     `json:"height"`
	BitRate            int64   `json:"bitrate"`
	FrameRate          float64 `json:"frame_rate"`
	PresetSpeed        string  `json:"preset_speed"`
	AudioCodec         string  `json:"audio_codec"`
	AudioBitRate       int     `json:"audio_bitrate"`
	HardwareAcceleration bool  `json:"hardware_acceleration"`
	PreferredDevice    int     `json:"preferred_device"`
}

// NegotiatedCodec holds the result of codec negotiation.
type NegotiatedCodec struct {
	Codec          string `json:"codec"`
	Acceleration   string `json:"acceleration"`
	FallbackReason string `json:"fallback_reason,omitempty"`
	IsSoftware     bool   `json:"is_software"`
}

// NegotiationResult holds the outcome of the codec negotiation process.
type NegotiationResult struct {
	SelectedPreset    *EncodingPreset   `json:"selected_preset"`
	NegotiatedCodec   *NegotiatedCodec  `json:"negotiated_codec"`
	ValidationErrors  []string          `json:"validation_errors,omitempty"`
	WarningMessages   []string          `json:"warning_messages,omitempty"`
}

// Negotiator selects the actual codec and acceleration method based on preset and hardware map.
type Negotiator struct {
	profile *HardwareProfile
}

// NewNegotiator creates a new codec negotiator.
func NewNegotiator(profile *HardwareProfile) *Negotiator {
	if profile == nil {
		profile = &HardwareProfile{
			Codecs:      make(CodecMap),
			LastUpdated: time.Now(),
		}
	}
	return &Negotiator{
		profile: profile,
	}
}

// Negotiate selects the codec and acceleration method for the given preset.
func (n *Negotiator) Negotiate(preset *EncodingPreset) *NegotiationResult {
	result := &NegotiationResult{
		SelectedPreset: preset,
		ValidationErrors: make([]string, 0),
		WarningMessages: make([]string, 0),
	}

	if preset == nil {
		result.ValidationErrors = append(result.ValidationErrors, "nil preset provided")
		return result
	}

	presetTargetCodec := normalizeCodec(preset.TargetCodec)
	result.NegotiatedCodec = &NegotiatedCodec{
		Codec:          presetTargetCodec,
		Acceleration:   "sw",
		IsSoftware:     true,
	}

	if !preset.HardwareAcceleration {
		result.WarningMessages = append(result.WarningMessages, "hardware acceleration not requested, using software encoding")
		return result
	}

	codecAccels := n.profile.Codecs[presetTargetCodec]
	if len(codecAccels) == 0 {
		codecAccels = n.profile.Codecs[normalizeCodec(presetTargetCodec)]
	}

	if len(codecAccels) == 0 {
		result.WarningMessages = append(result.WarningMessages, fmt.Sprintf("no hardware acceleration available for codec %s, falling back to software", presetTargetCodec))
		return result
	}

	if preset.PreferredDevice >= 0 && preset.PreferredDevice < len(n.profile.Devices) {
		preferredDevice := n.profile.Devices[preset.PreferredDevice]
		if preferredDevice.Healthy && !preferredDevice.ThermalThrottled {
			for _, accel := range codecAccels {
				if accel == preferredDevice.Acceleration {
					result.NegotiatedCodec.Acceleration = accel
					result.NegotiatedCodec.IsSoftware = false
					return result
				}
			}
		}
	}

	// Prioritize hardware encoders
	bestAccel := ""
	priorities := []string{"cuda", "qsv", "amf", "vaapi", "videotoolbox"}
	for _, p := range priorities {
		for _, accel := range codecAccels {
			if accel == p {
				bestAccel = accel
				break
			}
		}
		if bestAccel != "" {
			break
		}
	}

	if bestAccel == "" {
		for _, accel := range codecAccels {
			if accel != "sw" {
				bestAccel = accel
				break
			}
		}
	}

	if bestAccel == "" {
		result.NegotiatedCodec.Acceleration = "sw"
		result.NegotiatedCodec.IsSoftware = true
		return result
	}

	result.NegotiatedCodec.Acceleration = bestAccel
	result.NegotiatedCodec.IsSoftware = false

	return result
}

// ValidatePreset checks the preset against hardware capabilities.
func (n *Negotiator) ValidatePreset(preset *EncodingPreset) []string {
	var errors []string

	if preset == nil {
		return []string{"nil preset"}
	}

	if preset.TargetCodec == "" {
		errors = append(errors, "missing target codec")
	}

	if preset.PresetSpeed == "" {
		preset.PresetSpeed = "medium"
	}

	if preset.QualityLevel <= 0 {
		preset.QualityLevel = 23
	}

	if preset.BitRate <= 0 {
		errors = append(errors, "invalid bitrate")
	}

	if preset.AudioCodec == "" {
		preset.AudioCodec = "aac"
	}

	if preset.AudioBitRate <= 0 {
		preset.AudioBitRate = 128
	}

	return errors
}

// TryFallback attempts to negotiate a fallback codec when the primary choice fails.
func (n *Negotiator) TryFallback(primaryPreset *EncodingPreset, negotiation *NegotiationResult) *NegotiationResult {
	if negotiation.NegotiatedCodec != nil && !negotiation.NegotiatedCodec.IsSoftware {
		return negotiation
	}

	fallbackCodec := selectFallbackCodec(primaryPreset.TargetCodec)
	if fallbackCodec == "" {
		return negotiation
	}

	fallbackPreset := *primaryPreset
	fallbackPreset.TargetCodec = fallbackCodec

	fallbackResult := n.Negotiate(&fallbackPreset)
	fallbackResult.WarningMessages = append(fallbackResult.WarningMessages, negotiation.WarningMessages...)
	fallbackResult.WarningMessages = append(fallbackResult.WarningMessages, fmt.Sprintf("primary codec %s unavailable, using fallback %s", primaryPreset.TargetCodec, fallbackCodec))
	fallbackResult.NegotiatedCodec.FallbackReason = fmt.Sprintf("primary codec %s unavailable", primaryPreset.TargetCodec)

	return fallbackResult
}

// primaryPreset is used for fallback codec selection.
var primaryPreset *EncodingPreset

// selectFallbackCodec returns a fallback codec based on the primary codec.
func selectFallbackCodec(primaryCodec string) string {
	switch primaryCodec {
	case "h.265", "av1":
		return "h.264"
	case "h.264":
		return "h.265"
	default:
		return "h.264"
	}
}

// normalizeCodec standardizes codec names.
func normalizeCodec(codec string) string {
	if codec == "" {
		return "h.264"
	}
	c := codec
	switch c {
	case "h264", "avc":
		return "h.264"
	case "h265", "h.265", "hevc":
		return "h.265"
	case "av1":
		return "av1"
	default:
		return c
	}
}
