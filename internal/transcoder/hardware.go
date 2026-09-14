package transcoder

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// HardwareProfile represents the available hardware encoding capabilities.
type HardwareProfile struct {
	Devices     []GPUDevice     `json:"devices"`
	Codecs      CodecMap        `json:"codecs"`
	LastUpdated time.Time       `json:"last_updated"`
	Error       string          `json:"error,omitempty"`
}

// GPUDevice represents a GPU device with its encoding capabilities.
type GPUDevice struct {
	ID            int             `json:"id"`
	Name          string          `json:"name"`
	Vendor        string          `json:"vendor"`
	MemoryMB      int             `json:"memory_mb"`
	Utilization   float64         `json:"utilization"`
	Temperature   float64         `json:"temperature"`
	ThermalThrottled bool         `json:"thermal_throttled"`
	Healthy       bool            `json:"healthy"`
	SupportedCodecs []string      `json:"supported_codecs"`
	Acceleration  string          `json:"acceleration"`
}

// CodecMap maps codec names to their supported acceleration methods.
type CodecMap map[string][]string

// DiscoveryResult represents the outcome of hardware discovery.
type DiscoveryResult struct {
	Profile      *HardwareProfile `json:"profile"`
	DiscoveryTime time.Duration   `json:"discovery_time"`
	DevicesFound int             `json:"devices_found"`
	Error        string          `json:"error,omitempty"`
}

// Discovery queries the system for available GPU devices and encoding capabilities.
func Discovery(ctx context.Context, binaryPath string, preferredDevice int) *DiscoveryResult {
	start := time.Now()
	result := &DiscoveryResult{
		Profile: &HardwareProfile{
			Codecs:      make(CodecMap),
			LastUpdated: time.Now(),
		},
	}

	if binaryPath == "" {
		binaryPath = "ffmpeg"
	}

	devices, err := enumerateGPUs(ctx, binaryPath)
	if err != nil {
		result.Error = err.Error()
		result.Profile.Error = err.Error()
		result.DiscoveryTime = time.Since(start)
		return result
	}

	var healthyDevices []GPUDevice
	for i := range devices {
		if devices[i].Acceleration != "sw" {
			devices[i].Healthy = HealthCheckWithBinary(ctx, binaryPath, devices[i].Acceleration, devices[i].ID)
		}
		if devices[i].Healthy && devices[i].Acceleration != "sw" {
			healthyDevices = append(healthyDevices, devices[i])
		}
	}

	if len(healthyDevices) == 0 {
		healthyDevices = append(healthyDevices, GPUDevice{
			ID:              0,
			Name:            "Software",
			Vendor:          "Generic",
			Acceleration:    "sw",
			SupportedCodecs: []string{"h.264", "h.265", "av1"},
			MemoryMB:        0,
			Healthy:         true,
		})
		result.DevicesFound = 0
	} else {
		result.DevicesFound = len(healthyDevices)
	}

	result.Profile.Devices = healthyDevices

	for _, codec := range getDefaultCodecs() {
		for _, device := range healthyDevices {
			if device.Healthy && !device.ThermalThrottled {
				for _, sc := range device.SupportedCodecs {
					if sc == codec {
						result.Profile.Codecs[codec] = appendUnique(result.Profile.Codecs[codec], device.Acceleration)
						break
					}
				}
			}
		}
	}

	result.Profile.Codecs["h.264"] = appendUnique(result.Profile.Codecs["h.264"], "sw")
	result.Profile.Codecs["h.265"] = appendUnique(result.Profile.Codecs["h.265"], "sw")
	result.Profile.Codecs["av1"] = appendUnique(result.Profile.Codecs["av1"], "sw")

	result.DiscoveryTime = time.Since(start)
	return result
}

// enumerateGPUs queries ffmpeg for hardware device information.
func enumerateGPUs(ctx context.Context, binaryPath string) ([]GPUDevice, error) {
	cmd := exec.CommandContext(ctx, binaryPath, "-hide_banner", "-hwaccels")
	var stdout strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stdout

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("enumerate hwaccels: %w", err)
	}

	output := stdout.String()
	return parseHwAccels(output), nil
}

// parseHwAccels parses ffmpeg hardware acceleration output into GPU devices.
func parseHwAccels(output string) []GPUDevice {
	var devices []GPUDevice
	seen := make(map[string]bool)

	lines := strings.Split(output, "\n")
	for _, line := range lines {
		trimmed := strings.ToLower(strings.TrimSpace(line))
		if trimmed == "" || strings.Contains(trimmed, ":") {
			continue
		}

		var (
			accel           string
			deviceName      string
			vendor          string
			supportedCodecs []string
		)

		switch {
		case strings.HasPrefix(trimmed, "cuda") || strings.HasPrefix(trimmed, "cuvid"):
			accel = "cuda"
			deviceName = "NVIDIA CUDA"
			vendor = "NVIDIA"
			supportedCodecs = []string{"h.264", "h.265", "av1"}
		case strings.HasPrefix(trimmed, "qsv"):
			accel = "qsv"
			deviceName = "Intel QuickSync"
			vendor = "Intel"
			supportedCodecs = []string{"h.264", "h.265", "av1"}
		case strings.HasPrefix(trimmed, "amf"):
			accel = "amf"
			deviceName = "AMD AMF"
			vendor = "AMD"
			supportedCodecs = []string{"h.264", "h.265", "av1"}
		case strings.HasPrefix(trimmed, "vaapi"):
			accel = "vaapi"
			deviceName = "Linux VAAPI"
			vendor = "Linux"
			supportedCodecs = []string{"h.264", "h.265", "av1"}
		case strings.HasPrefix(trimmed, "videotoolbox"):
			accel = "videotoolbox"
			deviceName = "Apple VideoToolbox"
			vendor = "Apple"
			supportedCodecs = []string{"h.264", "h.265", "av1"}
		}

		if accel != "" && !seen[accel] {
			seen[accel] = true
			devices = append(devices, GPUDevice{
				ID:               len(devices),
				Name:             deviceName,
				Vendor:           vendor,
				Acceleration:     accel,
				SupportedCodecs:  supportedCodecs,
				MemoryMB:         estimateMemory(accel),
				Utilization:      0,
				Temperature:      0,
				ThermalThrottled: false,
				Healthy:          true,
			})
		}
	}

	if len(devices) == 0 {
		devices = append(devices, GPUDevice{
			ID:              0,
			Name:            "Software",
			Vendor:          "Generic",
			Acceleration:    "sw",
			SupportedCodecs: []string{"h.264", "h.265", "av1"},
			MemoryMB:        0,
			Healthy:         true,
		})
	}

	return devices
}

// getSupportedCodecsForAccel returns the list of codecs supported by an acceleration method.
func getSupportedCodecsForAccel(accel string) []string {
	switch accel {
	case "cuda", "qsv", "amf", "vaapi", "videotoolbox":
		return []string{"h.264", "h.265", "av1"}
	default:
		return []string{"h.264", "h.265"}
	}
}

// estimateMemory returns an estimated GPU memory for an acceleration type.
func estimateMemory(accel string) int {
	switch accel {
	case "cuda", "amf":
		return 4096
	case "qsv", "vaapi":
		return 2048
	case "videotoolbox":
		return 0
	default:
		return 0
	}
}

// getDefaultCodecs returns the list of common codec names.
func getDefaultCodecs() []string {
	return []string{"h.264", "h.265", "av1"}
}

// appendUnique appends unique values to a string slice.
func appendUnique(slice []string, items ...string) []string {
	existing := make(map[string]bool)
	for _, s := range slice {
		existing[s] = true
	}
	for _, item := range items {
		if !existing[item] {
			slice = append(slice, item)
			existing[item] = true
		}
	}
	return slice
}

// HealthCheck verifies the health of a GPU device using default binary.
func HealthCheck(ctx context.Context, accel string, deviceID int) bool {
	return HealthCheckWithBinary(ctx, "ffmpeg", accel, deviceID)
}

// HealthCheckWithBinary verifies the health of a GPU device using the specified binary path.
func HealthCheckWithBinary(ctx context.Context, binaryPath string, accel string, deviceID int) bool {
	if accel == "sw" || accel == "" {
		return true
	}
	if binaryPath == "" {
		binaryPath = "ffmpeg"
	}

	testEncoder := ""
	switch accel {
	case "cuda":
		testEncoder = "h264_nvenc"
	case "qsv":
		testEncoder = "h264_qsv"
	case "amf":
		testEncoder = "h264_amf"
	case "vaapi":
		testEncoder = "h264_vaapi"
	case "videotoolbox":
		testEncoder = "h264_videotoolbox"
	}

	// 1. Try a test encode with 256x256 (supported across modern hardware encoders)
	if testEncoder != "" {
		cmd := exec.CommandContext(ctx, binaryPath, "-hide_banner", "-f", "lavfi", "-i", "color=c=black:s=256x256:d=0.04", "-c:v", testEncoder, "-f", "null", "-")
		if err := cmd.Run(); err == nil {
			return true
		}
	}

	// 2. Fallback: try initializing hardware device
	cmd := exec.CommandContext(ctx, binaryPath, "-hide_banner", "-init_hw_device", fmt.Sprintf("%s:%d", accel, deviceID), "-f", "lavfi", "-i", "color=c=black:s=256x256:d=0.04", "-f", "null", "-")
	err := cmd.Run()
	return err == nil
}

// RefreshProfile refreshes the hardware profile by re-discovering devices.
func RefreshProfile(ctx context.Context, binaryPath string, preferredDevice int) *DiscoveryResult {
	return Discovery(ctx, binaryPath, preferredDevice)
}
