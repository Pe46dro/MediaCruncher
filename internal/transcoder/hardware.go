package transcoder

import (
	"context"
	"strings"
	"sync"
	"time"

	"mediacruncher/internal/proc"
)

type HardwareProfile struct {
	HasNVENC    bool
	HasQuickSync bool
	HasAMF      bool
	HasVAAPI    bool
	Encoders    map[string]bool // e.g. "hevc_nvenc": true
}

var (
	cachedProfile *HardwareProfile
	profileMu     sync.RWMutex
)

// DetectHardwareCapabilities queries ffmpeg to discover supported hardware encoders.
func DetectHardwareCapabilities(ctx context.Context) *HardwareProfile {
	profileMu.RLock()
	if cachedProfile != nil {
		p := cachedProfile
		profileMu.RUnlock()
		return p
	}
	profileMu.RUnlock()

	profileMu.Lock()
	defer profileMu.Unlock()

	if cachedProfile != nil {
		return cachedProfile
	}

	profile := &HardwareProfile{
		Encoders: make(map[string]bool),
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	stdout, _, err := proc.RunCommand(ctx, "ffmpeg", "-encoders")
	if err == nil {
		out := string(stdout)
		for _, line := range strings.Split(out, "\n") {
			line = strings.TrimSpace(line)
			if strings.Contains(line, "nvenc") {
				profile.HasNVENC = true
			}
			if strings.Contains(line, "qsv") {
				profile.HasQuickSync = true
			}
			if strings.Contains(line, "amf") {
				profile.HasAMF = true
			}
			if strings.Contains(line, "vaapi") {
				profile.HasVAAPI = true
			}

			fields := strings.Fields(line)
			if len(fields) >= 2 {
				encoderName := fields[1]
				profile.Encoders[encoderName] = true
			}
		}
	}

	cachedProfile = profile
	return profile
}

// SelectEncoder resolves the optimal ffmpeg video encoder based on hardware availability.
func (p *HardwareProfile) SelectEncoder(targetCodec string, hwAccelPref string) (encoderName string, isHardware bool) {
	codec := strings.ToLower(targetCodec)
	pref := strings.ToLower(hwAccelPref)

	if pref != "cpu" && pref != "none" {
		// Try NVENC
		if (pref == "auto" || pref == "nvenc") && p.HasNVENC {
			switch codec {
			case "hevc", "h265":
				if p.Encoders["hevc_nvenc"] {
					return "hevc_nvenc", true
				}
			case "h264", "avc":
				if p.Encoders["h264_nvenc"] {
					return "h264_nvenc", true
				}
			case "av1":
				if p.Encoders["av1_nvenc"] {
					return "av1_nvenc", true
				}
			}
		}

		// Try QuickSync (Intel)
		if (pref == "auto" || pref == "qsv") && p.HasQuickSync {
			switch codec {
			case "hevc", "h265":
				if p.Encoders["hevc_qsv"] {
					return "hevc_qsv", true
				}
			case "h264", "avc":
				if p.Encoders["h264_qsv"] {
					return "h264_qsv", true
				}
			case "av1":
				if p.Encoders["av1_qsv"] {
					return "av1_qsv", true
				}
			}
		}

		// Try AMD AMF
		if (pref == "auto" || pref == "amf") && p.HasAMF {
			switch codec {
			case "hevc", "h265":
				if p.Encoders["hevc_amf"] {
					return "hevc_amf", true
				}
			case "h264":
				if p.Encoders["h264_amf"] {
					return "h264_amf", true
				}
			}
		}
	}

	// Fallback to high-quality software encoding
	switch codec {
	case "hevc", "h265":
		return "libx265", false
	case "av1":
		if p.Encoders["libsvtav1"] {
			return "libsvtav1", false
		}
		return "libaom-av1", false
	default:
		return "libx264", false
	}
}
