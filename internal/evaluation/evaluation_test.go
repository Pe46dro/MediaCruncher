package evaluation

import (
	"testing"

	"mediacruncher/internal/config"
)

func TestNormalizationAndRuleMatching(t *testing.T) {
	// Simulated probe output
	raw := &ProbeOutput{
		Format: ProbeFormat{
			FormatName: "matroska",
			Duration:   "3600.5",
			BitRate:    "12000000",
		},
		Streams: []ProbeStream{
			{
				Index:         0,
				CodecName:     "avc1", // should resolve to h264
				CodecType:     "video",
				Width:         1920,
				Height:        1080,
				PixFmt:        "yuv420p10le", // 10-bit
				ColorTransfer: "smpte2084",   // HDR10
			},
			{
				Index:         1,
				CodecName:     "truehd",
				CodecType:     "audio",
				Channels:      8, // 7.1
				ChannelLayout: "7.1",
				Tags:          map[string]string{"language": "eng", "title": "Dolby TrueHD Atmos"},
				Disposition:   map[string]int{"default": 1},
			},
			{
				Index:       2,
				CodecName:   "subrip",
				CodecType:   "subtitle",
				Tags:        map[string]string{"language": "eng"},
				Disposition: map[string]int{"default": 1, "forced": 0},
			},
		},
	}

	aliases := map[string]string{"avc1": "h264"}
	meta := Normalize(raw, "/movies/test.mkv", aliases)

	if meta.VideoCodec != "h264" {
		t.Fatalf("expected resolved video codec h264, got %s", meta.VideoCodec)
	}
	if meta.ResolutionTag != "1080p" {
		t.Fatalf("expected resolution 1080p, got %s", meta.ResolutionTag)
	}
	if !meta.IsHDR || meta.HDRFormat != "hdr10" {
		t.Fatalf("expected HDR10 detection, got is_hdr=%v, format=%s", meta.IsHDR, meta.HDRFormat)
	}
	if !meta.HasSurround || !meta.HasLossless {
		t.Fatalf("expected TrueHD 7.1 to be flagged as surround and lossless")
	}

	// Test Rule Engine
	ruleCfg := config.EvaluationConfig{
		DefaultAction: "transcode",
		DefaultPreset: "balanced-hevc",
		AudioPreservation: config.AudioPreservation{
			RetainSurroundTracks: true,
			CopyLosslessAudio:    true,
		},
		Rules: []config.RuleConfig{
			{
				Name:            "Transcode H.264 1080p",
				Priority:        50,
				Conditions:      map[string]string{"codec": "h264", "resolution": "1080p"},
				Action:          "transcode",
				Preset:          "balanced-hevc",
				AudioAction:     "copy",
				RetainSubtitles: true,
			},
		},
	}

	engine := NewRuleEngine(ruleCfg)
	decision := engine.Evaluate(meta)

	if decision.MatchedRule != "Transcode H.264 1080p" {
		t.Fatalf("expected rule match, got: %s", decision.MatchedRule)
	}
	if decision.Action != "transcode" {
		t.Fatalf("expected action transcode, got %s", decision.Action)
	}

	// Test Multi-stream Preservation Synthesis
	plan := SynthesizeStreamPlan(meta, decision, ruleCfg.AudioPreservation)
	if len(plan.MapArgs) == 0 {
		t.Fatalf("expected non-empty map args in stream plan")
	}

	// Verify TrueHD track is mapped and set to copy
	hasCopy := false
	for _, arg := range plan.AudioCodecArgs {
		if arg == "copy" {
			hasCopy = true
			break
		}
	}
	if !hasCopy {
		t.Fatalf("expected lossless TrueHD audio track to be preserved with 'copy'")
	}
}
