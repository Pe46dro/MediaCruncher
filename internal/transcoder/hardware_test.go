package transcoder

import (
	"context"
	"strings"
	"testing"
)

func TestParseHwAccels(t *testing.T) {
	// Sample output from Windows ffmpeg -hwaccels
	windowsOutput := `Hardware acceleration methods:
cuda
dxva2
qsv
d3d11va
opencl
vulkan
d3d12va
amf
`
	devices := parseHwAccels(windowsOutput)
	if len(devices) == 0 {
		t.Fatal("expected devices to be found, got 0")
	}

	hasCuda := false
	hasQsv := false
	hasAmf := false
	for _, d := range devices {
		if d.Acceleration == "cuda" {
			hasCuda = true
			if d.Vendor != "NVIDIA" {
				t.Errorf("expected NVIDIA vendor for cuda, got %s", d.Vendor)
			}
		}
		if d.Acceleration == "qsv" {
			hasQsv = true
		}
		if d.Acceleration == "amf" {
			hasAmf = true
		}
	}

	if !hasCuda {
		t.Error("expected cuda device to be detected")
	}
	if !hasQsv {
		t.Error("expected qsv device to be detected")
	}
	if !hasAmf {
		t.Error("expected amf device to be detected")
	}
}

func TestParseHwAccelsEmptyFallback(t *testing.T) {
	devices := parseHwAccels("")
	if len(devices) != 1 {
		t.Fatalf("expected 1 fallback device, got %d", len(devices))
	}
	if devices[0].Acceleration != "sw" {
		t.Errorf("expected sw fallback acceleration, got %s", devices[0].Acceleration)
	}
}

func TestDiscoveryFilterHealthy(t *testing.T) {
	ctx := context.Background()
	disc := Discovery(ctx, "ffmpeg", 0)
	if disc == nil || disc.Profile == nil {
		t.Fatal("expected discovery profile, got nil")
	}

	for _, dev := range disc.Profile.Devices {
		if !dev.Healthy {
			t.Errorf("found unhealthy device in profile: %s (%s)", dev.Name, dev.Acceleration)
		}
	}
}

func TestNegotiateH265WithCuda(t *testing.T) {
	profile := &HardwareProfile{
		Codecs: CodecMap{
			"h.265": []string{"cuda", "sw"},
			"h.264": []string{"cuda", "sw"},
		},
		Devices: []GPUDevice{
			{
				ID:           0,
				Name:         "NVIDIA CUDA",
				Acceleration: "cuda",
				Healthy:      true,
			},
		},
	}

	negotiator := NewNegotiator(profile)
	preset := &EncodingPreset{
		TargetCodec:          "h.265",
		QualityLevel:         23,
		HardwareAcceleration: true,
		PreferredDevice:      0,
	}

	result := negotiator.Negotiate(preset)
	if result.NegotiatedCodec == nil {
		t.Fatal("expected negotiated codec, got nil")
	}
	if result.NegotiatedCodec.Codec != "h.265" {
		t.Errorf("expected h.265 codec, got %s", result.NegotiatedCodec.Codec)
	}
	if result.NegotiatedCodec.Acceleration != "cuda" {
		t.Errorf("expected cuda acceleration, got %s", result.NegotiatedCodec.Acceleration)
	}
	if result.NegotiatedCodec.IsSoftware {
		t.Error("expected IsSoftware to be false")
	}
}

func TestNegotiateFallbackToSoftware(t *testing.T) {
	profile := &HardwareProfile{
		Codecs: CodecMap{
			"h.265": []string{"sw"},
		},
		Devices: []GPUDevice{
			{
				ID:           0,
				Name:         "Software",
				Acceleration: "sw",
				Healthy:      true,
			},
		},
	}

	negotiator := NewNegotiator(profile)
	preset := &EncodingPreset{
		TargetCodec:          "h.265",
		QualityLevel:         23,
		HardwareAcceleration: false,
	}

	result := negotiator.Negotiate(preset)
	if result.NegotiatedCodec == nil {
		t.Fatal("expected negotiated codec, got nil")
	}
	if result.NegotiatedCodec.Acceleration != "sw" {
		t.Errorf("expected sw acceleration, got %s", result.NegotiatedCodec.Acceleration)
	}
	if !result.NegotiatedCodec.IsSoftware {
		t.Error("expected IsSoftware to be true")
	}
}

func TestBuildCommandH265Nvenc(t *testing.T) {
	encoder := NewEncoder("ffmpeg", "/tmp/staging")
	job := &TranscodeJob{
		JobID:      "test-job",
		SourcePath: "/path/to/source.mp4",
		OutputPath: "/path/to/dest.mp4",
		StagingDir: "/tmp/staging",
		NegotiatedCodec: &NegotiatedCodec{
			Codec:        "h.265",
			Acceleration: "cuda",
			IsSoftware:   false,
		},
		Preset: &EncodingPreset{
			TargetCodec:          "h.265",
			QualityLevel:         23,
			PresetSpeed:          "medium",
			HardwareAcceleration: true,
			PreferredDevice:      0,
		},
	}

	cmd, err := encoder.buildCommand(context.Background(), job, "/tmp/staging/dest.mp4")
	if err != nil {
		t.Fatalf("buildCommand failed: %v", err)
	}

	args := strings.Join(cmd.Args, " ")

	if !strings.Contains(args, "-hwaccel cuda") {
		t.Errorf("expected -hwaccel cuda in args: %s", args)
	}
	if !strings.Contains(args, "-c:v hevc_nvenc") {
		t.Errorf("expected -c:v hevc_nvenc in args: %s", args)
	}
	if !strings.Contains(args, "-cq 23") {
		t.Errorf("expected -cq 23 for nvenc in args: %s", args)
	}
	if !strings.Contains(args, "-tag:v hvc1") {
		t.Errorf("expected -tag:v hvc1 for mp4 hevc in args: %s", args)
	}
}

func TestBuildCommandH265Software(t *testing.T) {
	encoder := NewEncoder("ffmpeg", "/tmp/staging")
	job := &TranscodeJob{
		JobID:      "test-job",
		SourcePath: "/path/to/source.mkv",
		OutputPath: "/path/to/dest.mkv",
		StagingDir: "/tmp/staging",
		NegotiatedCodec: &NegotiatedCodec{
			Codec:        "h.265",
			Acceleration: "sw",
			IsSoftware:   true,
		},
		Preset: &EncodingPreset{
			TargetCodec:          "h.265",
			QualityLevel:         23,
			PresetSpeed:          "medium",
			HardwareAcceleration: false,
		},
	}

	cmd, err := encoder.buildCommand(context.Background(), job, "/tmp/staging/dest.mkv")
	if err != nil {
		t.Fatalf("buildCommand failed: %v", err)
	}

	args := strings.Join(cmd.Args, " ")

	if strings.Contains(args, "-hwaccel") {
		t.Errorf("unexpected -hwaccel in sw args: %s", args)
	}
	if !strings.Contains(args, "-c:v libx265") {
		t.Errorf("expected -c:v libx265 in args: %s", args)
	}
	if !strings.Contains(args, "-crf 23") {
		t.Errorf("expected -crf 23 in args: %s", args)
	}
}
