package app

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/wsdbb72/MusicOps/native/core/internal/trackersearch"
)

var (
	qualityBitDepthPattern   = regexp.MustCompile(`(?i)(?:\b|/)(16|24|32)[ -]?bit\b`)
	qualitySampleRatePattern = regexp.MustCompile(`(?i)(?:\b|/)(44\.1|48|88\.2|96|176\.4|192|352\.8|384)\s?(?:k(?:hz)?|khz)\b|(?:^|[/\s])(44\.1|48|88\.2|96|176\.4|192|352\.8|384)(?:/|\b)`)
	qualityBitratePattern    = regexp.MustCompile(`(?i)(?:\b|_)(64|96|128|160|192|224|256|320|1411)\s?(?:kbps|k)\b`)
)

type candidateQualityRank struct {
	Lossless   int
	BitDepth   int
	SampleRate int
	BitRate    int
	Codec      int
	Free       int
	Seeders    int
	SiteOrder  int
}

func candidateQuality(plan SearchPlan, site string, candidate trackersearch.Candidate) (candidateQualityRank, bool) {
	text := strings.ToLower(strings.Join([]string{
		candidate.Title, mediaText(candidate.Media, "format"), mediaText(candidate.Media, "encoding"),
		mediaText(candidate.Media, "media"),
	}, " "))
	format := strings.ToUpper(strings.TrimSpace(mediaText(candidate.Media, "format")))
	encoding := strings.ToLower(mediaText(candidate.Media, "encoding"))
	lossless := isLosslessCandidateFormat(format, encoding, text)
	rank := candidateQualityRank{SiteOrder: siteOrder(site)}
	switch format {
	case "DSD":
		rank.Codec = 9
	case "FLAC":
		rank.Codec = 8
	case "ALAC":
		rank.Codec = 7
	case "WAV", "AIFF":
		rank.Codec = 6
	case "OPUS":
		rank.Codec = 5
	case "AAC", "OGG", "VORBIS":
		rank.Codec = 4
	case "MP3":
		rank.Codec = 3
	}
	if lossless || format == "" && strings.Contains(text, "lossless") {
		rank.Lossless, rank.BitDepth, rank.SampleRate = 1, 16, 44100
	}
	if bits := firstQualityInt(qualityBitDepthPattern, text); bits > 0 {
		rank.BitDepth = bits
	}
	if bits := numericMedia(candidate.Media, "bitDepth", "bitsPerSample"); bits > 0 {
		rank.BitDepth = bits
	}
	if rate := firstQualityRate(text); rate > 0 {
		rank.SampleRate = rate
	}
	if rate := numericMedia(candidate.Media, "sampleRate", "samplingRate"); rate > 0 {
		rank.SampleRate = rate
	}
	if !lossless {
		rank.BitRate = lossyBitrate(text)
		if bitrate := numericMedia(candidate.Media, "bitrate", "bitRate"); bitrate > 0 {
			if bitrate < 1000 {
				bitrate *= 1000
			}
			rank.BitRate = bitrate
		}
	}
	seeders := 0
	if candidate.Seeders != nil {
		seeders = *candidate.Seeders
	}
	rank.Seeders = seeders
	free := candidateIsFree(candidate.SiteFlags)
	if free {
		rank.Free = 1
	}
	canToken, canTokenKnown := optionalMediaFlag(candidate.SiteFlags, "canUseToken")
	tokenSite := site == "dicmusic" || site == "orpheus"
	if tokenSite {
		// FL is mandatory on these sites. A non-free row that explicitly
		// cannot use the FL button, cannot fund the reservation, or has no
		// seeders must not block a lower-quality candidate that can complete.
		if !free && canTokenKnown && !canToken {
			return rank, false
		}
		if !free && seeders <= 0 {
			return rank, false
		}
	} else if site != "open.cd" && seeders <= 0 {
		return rank, false
	}
	return rank, true
}

func mediaText(media map[string]any, key string) string {
	if media == nil {
		return ""
	}
	switch value := media[key].(type) {
	case string:
		return strings.TrimSpace(value)
	case int:
		return strconv.Itoa(value)
	case int64:
		return strconv.FormatInt(value, 10)
	case float64:
		return strconv.FormatFloat(value, 'f', -1, 64)
	default:
		return ""
	}
}

func numericMedia(media map[string]any, keys ...string) int {
	for _, key := range keys {
		value := mediaText(media, key)
		if value == "" {
			continue
		}
		value = strings.TrimSpace(strings.TrimSuffix(strings.ToLower(value), "hz"))
		if parsed, err := strconv.Atoi(value); err == nil {
			return parsed
		}
	}
	return 0
}

func isLosslessCandidateFormat(format, encoding, text string) bool {
	if strings.Contains(encoding, "lossless") || strings.Contains(text, "lossless") || strings.Contains(text, "无损") {
		return true
	}
	switch format {
	case "FLAC", "ALAC", "WAV", "AIFF", "DSD":
		return true
	}
	if strings.Contains(text, "hi-res") || strings.Contains(text, "hires") || strings.Contains(text, "24/96") || strings.Contains(text, "24/192") {
		return !strings.Contains(text, "mp3")
	}
	return false
}

func firstQualityInt(pattern *regexp.Regexp, text string) int {
	match := pattern.FindStringSubmatch(text)
	if len(match) < 2 {
		return 0
	}
	value, _ := strconv.Atoi(match[1])
	return value
}

func firstQualityRate(text string) int {
	match := qualitySampleRatePattern.FindStringSubmatch(text)
	for index := 1; index < len(match); index++ {
		if match[index] == "" {
			continue
		}
		value, _ := strconv.ParseFloat(match[index], 64)
		return int(value * 1000)
	}
	return 0
}

func lossyBitrate(text string) int {
	if match := qualityBitratePattern.FindStringSubmatch(text); len(match) > 1 {
		value, _ := strconv.Atoi(match[1])
		return value * 1000
	}
	upper := strings.ToUpper(text)
	switch {
	case strings.Contains(upper, "V0") || strings.Contains(upper, "VBR V0"):
		return 245000
	case strings.Contains(upper, "V1"):
		return 225000
	case strings.Contains(upper, "V2"):
		return 190000
	case strings.Contains(upper, "INSANE"):
		return 320000
	case strings.Contains(upper, "EXTREME"):
		return 245000
	case strings.Contains(upper, "STANDARD"):
		return 190000
	}
	return 0
}

func candidateIsFree(flags map[string]any) bool {
	for _, key := range []string{"isPersonalFreeleech", "isFreeleech", "personal_freeleech", "freeleech"} {
		if mediaFlag(flags, key) {
			return true
		}
	}
	return false
}

func optionalMediaFlag(flags map[string]any, key string) (bool, bool) {
	if flags == nil {
		return false, false
	}
	value, found := flags[key]
	if !found {
		return false, false
	}
	switch item := value.(type) {
	case bool:
		return item, true
	case float64:
		return item != 0, true
	case int:
		return item != 0, true
	case string:
		item = strings.ToLower(strings.TrimSpace(item))
		return item == "1" || item == "true" || item == "yes", true
	default:
		return false, true
	}
}

func mediaFlag(flags map[string]any, key string) bool {
	if flags == nil {
		return false
	}
	switch value := flags[key].(type) {
	case bool:
		return value
	case float64:
		return value != 0
	case int:
		return value != 0
	case string:
		value = strings.ToLower(strings.TrimSpace(value))
		return value == "1" || value == "true" || value == "yes"
	default:
		return false
	}
}

func siteOrder(site string) int {
	switch site {
	case "open.cd":
		return 0
	case "dicmusic":
		return 1
	case "orpheus":
		return 2
	case "redacted":
		return 3
	default:
		return 9
	}
}

func candidateQualityLabel(rank candidateQualityRank, candidate trackersearch.Candidate) string {
	format := strings.ToUpper(strings.TrimSpace(mediaText(candidate.Media, "format")))
	if rank.Lossless > 0 {
		label := format + " 无损"
		if rank.BitDepth > 0 {
			label += fmt.Sprintf(" %d-bit", rank.BitDepth)
		}
		if rank.SampleRate > 0 {
			label += fmt.Sprintf("/%g kHz", float64(rank.SampleRate)/1000)
		}
		return strings.TrimSpace(label)
	}
	if rank.BitRate > 0 {
		return fmt.Sprintf("%s %d kbps", format, (rank.BitRate+500)/1000)
	}
	if format != "" {
		return format
	}
	return mediaText(candidate.Media, "encoding")
}

func compareCandidateQuality(a, b candidateQualityRank) int {
	av := []int{a.Lossless, a.BitDepth, a.SampleRate, a.BitRate, a.Codec, a.Free, a.Seeders, -a.SiteOrder}
	bv := []int{b.Lossless, b.BitDepth, b.SampleRate, b.BitRate, b.Codec, b.Free, b.Seeders, -b.SiteOrder}
	for i := range av {
		if av[i] != bv[i] {
			return av[i] - bv[i]
		}
	}
	return 0
}
