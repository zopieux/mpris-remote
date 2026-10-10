package player

// State represents the unified state of the media player and audio output.
type State struct {
	State  string // "playing", "paused", "stopped"
	Title  string
	Artist string
	Player string
	Volume   int     // 0 to 100
	Mute     bool
	Position float64 // in seconds
	Duration float64 // in seconds
}

// Action represents commands sent to MPRIS players.
type Action int

const (
	ActionPlay Action = iota
	ActionPause
	ActionStop
	ActionPrevious
	ActionNext
	ActionToggle
)
