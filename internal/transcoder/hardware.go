package transcoder

import (
	"context"
	"os"
	"runtime"
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
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				encoderName := fields[1]
				profile.Encoders[encoderName] = true
			}
		}

		// Verify actual hardware initialization capability (especially in Docker/containers)
		hwEncoders := []string{
			"hevc_nvenc", "h264_nvenc", "av1_nvenc",
			"hevc_qsv", "h264_qsv", "av1_qsv",
			"hevc_amf", "h264_amf",
			"hevc_vaapi", "h264_vaapi",
		}
		for _, enc := range hwEncoders {
			if profile.Encoders[enc] {
				if !isEncoderFunctional(ctx, enc) {
					delete(profile.Encoders, enc)
				}
			}
		}

		profile.HasNVENC = profile.Encoders["hevc_nvenc"] || profile.Encoders["h264_nvenc"] || profile.Encoders["av1_nvenc"]
		profile.HasQuickSync = profile.Encoders["hevc_qsv"] || profile.Encoders["h264_qsv"] || profile.Encoders["av1_qsv"]
		profile.HasAMF = profile.Encoders["hevc_amf"] || profile.Encoders["h264_amf"]
		profile.HasVAAPI = profile.Encoders["hevc_vaapi"] || profile.Encoders["h264_vaapi"]
	}

	cachedProfile = profile
	return profile
}

func isEncoderFunctional(ctx context.Context, encoder string) bool {
	if runtime.GOOS == "linux" {
		// Quick device existence checks on Linux / Docker
		if strings.Contains(encoder, "qsv") || strings.Contains(encoder, "vaapi") {
			if _, err := os.Stat("/dev/dri"); os.IsNotExist(err) {
				return false
			}
		}
		if strings.Contains(encoder, "nvenc") {
			hasNvidia := false
			if _, err := os.Stat("/dev/dxg"); err == nil {
				hasNvidia = true // WSL2 Direct3D / NVIDIA Passthrough
			} else if _, err := os.Stat("/dev/nvidiactl"); err == nil {
				hasNvidia = true
			} else if _, err := os.Stat("/dev/nvidia0"); err == nil {
				hasNvidia = true
			}
			if !hasNvidia {
				return false
			}
		}
	}

	testCtx, cancel := context.WithTimeout(ctx, 2500*time.Millisecond)
	defer cancel()

	var cmdArgs []string
	if strings.Contains(encoder, "vaapi") {
		// VAAPI requires initializing the hw device and uploading frames
		renderDevice := "/dev/dri/renderD128"
		if _, err := os.Stat(renderDevice); os.IsNotExist(err) {
			renderDevice = ""
		}
		if renderDevice != "" {
			cmdArgs = []string{"-y", "-init_hw_device", "vaapi=va:" + renderDevice, "-filter_hw_device", "va", "-f", "lavfi", "-i", "testsrc=duration=1:size=256x256:rate=1", "-vf", "format=nv12,hwupload", "-c:v", encoder, "-f", "null", "-"}
		} else {
			cmdArgs = []string{"-y", "-init_hw_device", "vaapi=va", "-filter_hw_device", "va", "-f", "lavfi", "-i", "testsrc=duration=1:size=256x256:rate=1", "-vf", "format=nv12,hwupload", "-c:v", encoder, "-f", "null", "-"}
		}
	} else {
		cmdArgs = []string{"-y", "-f", "lavfi", "-i", "testsrc=duration=1:size=256x256:rate=1", "-c:v", encoder, "-f", "null", "-"}
	}

	_, _, err := proc.RunCommand(testCtx, "ffmpeg", cmdArgs...)
	return err == nil
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

		// Try VAAPI (Intel / AMD GPU on Linux)
		if (pref == "auto" || pref == "vaapi") && p.HasVAAPI {
			switch codec {
			case "hevc", "h265":
				if p.Encoders["hevc_vaapi"] {
					return "hevc_vaapi", true
				}
				// Hardware Fallback: if HEVC hardware encoder is not supported by the GPU (e.g. Braswell/Apollo Lake),
				// fallback to available H264 hardware encoder rather than crashing CPU with libx265
				if p.Encoders["h264_vaapi"] {
					return "h264_vaapi", true
				}
			case "h264", "avc":
				if p.Encoders["h264_vaapi"] {
					return "h264_vaapi", true
				}
			case "av1":
				if p.Encoders["av1_vaapi"] {
					return "av1_vaapi", true
				}
				if p.Encoders["hevc_vaapi"] {
					return "hevc_vaapi", true
				}
				if p.Encoders["h264_vaapi"] {
					return "h264_vaapi", true
				}
			}
		}
	}

	// Fallback to software encoding if no hardware matches
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
