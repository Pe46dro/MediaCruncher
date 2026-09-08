package transcoder

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
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

	result.DevicesFound = len(devices)
	result.Profile.Devices = devices

	for _, codec := range getDefaultCodecs() {
		for _, device := range devices {
			if device.Healthy && !device.ThermalThrottled {
				result.Profile.Codecs[codec] = append(result.Profile.Codecs[codec], device.Acceleration)
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

	hwAccelPattern := regexp.MustCompile(`^\s*(cuda|cuvid|qsv|dxva2|d3d11va|videotoolbox|vaapi)\s+(\w+)\s*$`)

	lines := strings.Split(output, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if matches := hwAccelPattern.FindStringSubmatch(line); matches != nil {
			accel := matches[1]
			deviceName := "default"
			if accel == "cuda" {
				deviceName = "NVIDIA CUDA"
			} else if accel == "qsv" {
				deviceName = "Intel QuickSync"
			} else if accel == "dxva2" {
				deviceName = "Windows DXVA2"
			} else if accel == "d3d11va" {
				deviceName = "Windows D3D11"
			} else if accel == "videotoolbox" {
				deviceName = "Apple VideoToolbox"
			} else if accel == "vaapi" {
				deviceName = "Linux VAAPI"
			}

			supportedCodecs := getSupportedCodecsForAccel(accel)

			devices = append(devices, GPUDevice{
				ID:                len(devices),
				Name:              deviceName,
				Acceleration:      accel,
				SupportedCodecs:   supportedCodecs,
				MemoryMB:          estimateMemory(accel),
				Utilization:       0,
				Temperature:       0,
				ThermalThrottled:  false,
				Healthy:           true,
			})
		}
	}

	if len(devices) == 0 {
		devices = append(devices, GPUDevice{
			ID:             0,
			Name:           "Software",
			Acceleration:   "sw",
			SupportedCodecs: []string{"h.264", "h.265", "av1"},
			MemoryMB:       0,
			Healthy:        true,
		})
	}

	return devices
}

// getSupportedCodecsForAccel returns the list of codecs supported by an acceleration method.
func getSupportedCodecsForAccel(accel string) []string {
	switch accel {
	case "cuda":
		return []string{"h.264", "h.265"}
	case "qsv":
		return []string{"h.264", "h.265"}
	case "dxva2":
		return []string{"h.264", "h.265"}
	case "d3d11va":
		return []string{"h.264", "h.265"}
	case "videotoolbox":
		return []string{"h.264", "h.265", "av1"}
	case "vaapi":
		return []string{"h.264", "h.265"}
	default:
		return []string{"h.264", "h.265"}
	}
}

// estimateMemory returns an estimated GPU memory for an acceleration type.
func estimateMemory(accel string) int {
	switch accel {
	case "cuda":
		return 4096
	case "qsv", "dxva2", "d3d11va":
		return 2048
	case "videotoolbox":
		return 0
	case "vaapi":
		return 2048
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

// HealthCheck verifies the health of a GPU device.
func HealthCheck(ctx context.Context, accel string, deviceID int) bool {
	if accel == "sw" {
		return true
	}

	cmd := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-hwaccel", accel, "-hwaccel_device", fmt.Sprintf("%d", deviceID), "-f", "null", "-")
	err := cmd.Run()
	return err == nil
}

// RefreshProfile refreshes the hardware profile by re-discovering devices.
func RefreshProfile(ctx context.Context, binaryPath string, preferredDevice int) *DiscoveryResult {
	return Discovery(ctx, binaryPath, preferredDevice)
}
