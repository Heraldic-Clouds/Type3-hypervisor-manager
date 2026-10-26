// Package protocol defines MEA Stream Protocol v1 messages (docs/PROTOCOL.md).
package protocol

import (
	"encoding/json"
	"slices"
)

const (
	Version = "mea-stream/1"

	LabelControl = "mea-control"
	LabelKey     = "mea-key"
	LabelRT      = "mea-rt"
)

// Error codes (§7).
const (
	CodeNormal        = 1000
	CodeBadToken      = 4001
	CodeForbidden     = 4003
	CodeTakenOver     = 4009
	CodeCapacity      = 4029
	CodeAgentDown     = 4100
	CodeCaptureFailed = 4101
	CodeEncoderFailed = 4102
	CodeVersion       = 4200
	CodeMalformed     = 4400
)

// Codec names used in hello/welcome and on the agent link.
const (
	H264 = "h264"
	H265 = "h265"
	AV1  = "av1"
)

// ServerPreference is the order in which the gateway picks a codec (§4).
var ServerPreference = []string{AV1, H265, H264}

// SelectCodec returns the first server-preferred codec that every list
// contains. H.264 is mandatory, so it is the fallback when it is common.
func SelectCodec(lists ...[]string) string {
	for _, c := range ServerPreference {
		ok := true
		for _, l := range lists {
			if !slices.Contains(l, c) {
				ok = false
				break
			}
		}
		if ok {
			return c
		}
	}
	return ""
}

// Profile is a quality target (§5).
type Profile struct {
	Name      string `json:"name"`
	W         int    `json:"w"`
	H         int    `json:"h"`
	FPS       int    `json:"fps"`
	StartKbps int    `json:"startKbps"`
	MaxKbps   int    `json:"maxKbps"`
}

var Profiles = map[string]Profile{
	"economy":  {"economy", 1280, 720, 30, 3000, 6000},
	"balanced": {"balanced", 1920, 1080, 60, 8000, 15000},
	"high":     {"high", 1920, 1080, 60, 12000, 25000},
}

var profileRank = map[string]int{"economy": 0, "balanced": 1, "high": 2}

// ClampProfile returns the requested profile limited to the token's maximum.
func ClampProfile(requested, max string) Profile {
	r, ok := profileRank[requested]
	if !ok {
		r = profileRank["balanced"]
	}
	m, ok := profileRank[max]
	if !ok {
		m = profileRank["balanced"]
	}
	if r > m {
		r = m
	}
	for n, i := range profileRank {
		if i == r {
			return Profiles[n]
		}
	}
	return Profiles["balanced"]
}

// Control is a §3.1 control message. Fields are optional depending on T.
type Control struct {
	T string `json:"t"`

	// hello / welcome
	V       json.RawMessage `json:"v,omitempty"`
	Codecs  []string        `json:"codecs,omitempty"`
	Screen  *Screen         `json:"screen,omitempty"`
	Gamepad bool            `json:"gamepad,omitempty"`
	SID     string          `json:"sid,omitempty"`
	Codec   string          `json:"codec,omitempty"`
	Audio   bool            `json:"audio,omitempty"`
	Caps    []string        `json:"caps,omitempty"`

	// configure / configured / welcome
	Profile string `json:"profile,omitempty"`
	W       int    `json:"w,omitempty"`
	H       int    `json:"h,omitempty"`
	FPS     int    `json:"fps,omitempty"`
	MaxKbps int    `json:"maxKbps,omitempty"`
	Kbps    int    `json:"kbps,omitempty"`

	// ping / pong
	N  uint64 `json:"n,omitempty"`
	TS int64  `json:"ts,omitempty"`

	// stats (client)
	DecodeMs float64 `json:"decodeMs,omitempty"`
	Dropped  int     `json:"dropped,omitempty"`
	JitterMs float64 `json:"jitterMs,omitempty"`
	LossPct  float64 `json:"lossPct,omitempty"`
	RTTMs    float64 `json:"rttMs,omitempty"`

	// sstats (server)
	EncMs      float64 `json:"encMs,omitempty"`
	CapMs      float64 `json:"capMs,omitempty"`
	TargetKbps int     `json:"targetKbps,omitempty"`
	Idle       bool    `json:"idle,omitempty"`
	GPU        float64 `json:"gpu,omitempty"`
	NVENC      float64 `json:"nvenc,omitempty"`

	// error / bye
	Code   int    `json:"code,omitempty"`
	Msg    string `json:"msg,omitempty"`
	Fatal  bool   `json:"fatal,omitempty"`
	Reason string `json:"reason,omitempty"`
}

type Screen struct {
	W   int     `json:"w"`
	H   int     `json:"h"`
	DPR float64 `json:"dpr"`
	Hz  int     `json:"hz"`
}

// Versions decodes hello.v, which is a list of version strings.
func (c Control) Versions() []string {
	var v []string
	if json.Unmarshal(c.V, &v) == nil {
		return v
	}
	var s string
	if json.Unmarshal(c.V, &s) == nil {
		return []string{s}
	}
	return nil
}

// NegotiateVersion returns Version if the client offers it.
func NegotiateVersion(offered []string) (string, bool) {
	if slices.Contains(offered, Version) {
		return Version, true
	}
	return "", false
}

// VersionJSON encodes a chosen version for welcome.v.
func VersionJSON(v string) json.RawMessage { b, _ := json.Marshal(v); return b }
