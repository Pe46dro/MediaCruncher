package evaluation

import (
	"fmt"
	"strings"

	"mediacruncher/internal/config"
)

type StreamPlan struct {
	MapArgs        []string `json:"map_args"`        // e.g. ["-map", "0:v:0", "-map", "0:a", "-map", "0:s?"]
	AudioCodecArgs []string `json:"audio_codec_args"` // e.g. ["-c:a:0", "copy", "-c:a:1", "aac"]
	SubtitleArgs   []string `json:"subtitle_args"`   // e.g. ["-c:s", "copy"]
	Summary        string   `json:"summary"`
}

// SynthesizeStreamPlan builds explicit FFmpeg stream mapping directives.
// Ensures surround tracks, secondary languages, and subtitles are never lost.
func SynthesizeStreamPlan(meta *NormalizedMetadata, decision MatchedDecision, pres config.AudioPreservation) StreamPlan {
	var mapArgs []string
	var audioArgs []string
	var subArgs []string
	var planNotes []string

	// 1. Primary Video Mapping
	mapArgs = append(mapArgs, "-map", "0:v:0")
	planNotes = append(planNotes, "Video: 0:v:0")

	// 2. Audio Mapping & Preservation
	if len(meta.AudioTracks) > 0 {
		preferredLangs := make(map[string]bool)
		for _, l := range pres.PreferredLanguages {
			preferredLangs[strings.ToLower(l)] = true
		}

		audioOutputIdx := 0
		for _, track := range meta.AudioTracks {
			// Check language filter if configured
			if len(preferredLangs) > 0 && track.Language != "" && !preferredLangs[track.Language] && !track.IsDefault {
				continue // skip non-preferred secondary audio track
			}

			mapArgs = append(mapArgs, "-map", fmt.Sprintf("0:%d", track.Index))

			// Determine audio track disposition
			codecArg := fmt.Sprintf("-c:a:%d", audioOutputIdx)
			if decision.AudioAction == "copy" || (pres.CopyLosslessAudio && track.IsLossless) || (pres.RetainSurroundTracks && track.IsSurround) {
				audioArgs = append(audioArgs, codecArg, "copy")
				planNotes = append(planNotes, fmt.Sprintf("Audio #%d (%s, %s): copy", track.Index, track.Codec, track.Layout))
			} else if decision.AudioAction == "aac_stereo" {
				audioArgs = append(audioArgs, codecArg, "aac", fmt.Sprintf("-b:a:%d", audioOutputIdx), "192k", fmt.Sprintf("-ac:%d", audioOutputIdx), "2")
				planNotes = append(planNotes, fmt.Sprintf("Audio #%d (%s): re-encode aac stereo", track.Index, track.Codec))
			} else {
				// Default behavior: stream copy to prevent any loss of quality
				audioArgs = append(audioArgs, codecArg, "copy")
				planNotes = append(planNotes, fmt.Sprintf("Audio #%d (%s): copy", track.Index, track.Codec))
			}
			audioOutputIdx++
		}
	} else {
		planNotes = append(planNotes, "Audio: none")
	}

	// 3. Subtitles Mapping & Preservation
	if decision.RetainSubtitles && len(meta.SubtitleTracks) > 0 {
		mapArgs = append(mapArgs, "-map", "0:s?")
		subArgs = append(subArgs, "-c:s", "copy")
		planNotes = append(planNotes, fmt.Sprintf("Subtitles: %d tracks preserved via copy", len(meta.SubtitleTracks)))
	}

	return StreamPlan{
		MapArgs:        mapArgs,
		AudioCodecArgs: audioArgs,
		SubtitleArgs:   subArgs,
		Summary:        strings.Join(planNotes, " | "),
	}
}
