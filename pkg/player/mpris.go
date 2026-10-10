package player

import (
	"log/slog"
	"maps"
	"slices"
	"strings"

	"github.com/godbus/dbus/v5"
	"github.com/leberKleber/go-mpris"
)

const (
	DBusPrefix = "org.mpris.MediaPlayer2."
	DBusIface  = DBusPrefix + "Player"
	DBusPath   = "/org/mpris/MediaPlayer2"
)

type PlayerInfo struct {
	State    string
	Title    string
	Artist   string
	Player   string
	Position float64 // in seconds
	Duration float64 // in seconds
	TrackID  dbus.ObjectPath
}

type MPRISMonitor struct {
	conn       *dbus.Conn
	actionChan chan Action
	seekChan   chan float64
}

func NewMPRISMonitor() (*MPRISMonitor, error) {
	conn, err := dbus.SessionBus()
	if err != nil {
		return nil, err
	}
	return &MPRISMonitor{
		conn:       conn,
		actionChan: make(chan Action, 8),
		seekChan:   make(chan float64, 4),
	}, nil
}

func (m *MPRISMonitor) SendAction(action Action) {
	select {
	case m.actionChan <- action:
	default:
	}
}

func (m *MPRISMonitor) Seek(posSec float64) {
	select {
	case m.seekChan <- posSec:
	default:
	}
}

func FindActivePlayer(players map[string]PlayerInfo) string {
	rank := func(n string) int {
		switch players[n].State {
		case "playing":
			return 2
		case "paused":
			return 1
		default:
			return 0
		}
	}
	keys := slices.Collect(maps.Keys(players))
	if len(keys) == 0 {
		return ""
	}
	return slices.MaxFunc(keys, func(a, b string) int {
		return rank(a) - rank(b)
	})
}

func parsePlayerState(p mpris.Player) *PlayerInfo {
	ps, err := p.PlaybackStatus()
	if err != nil || ps == "" {
		return nil
	}
	info := &PlayerInfo{}
	switch ps {
	case mpris.PlaybackStatusPlaying:
		info.State = "playing"
	case mpris.PlaybackStatusPaused:
		info.State = "paused"
	case mpris.PlaybackStatusStopped:
		return nil
	default:
		return nil
	}

	metadata, err := p.Metadata()
	if err == nil {
		if artists, err := metadata.XESAMArtist(); err == nil && len(artists) > 0 {
			info.Artist = strings.Join(artists, ", ")
		}
		if title, err := metadata.XESAMTitle(); err == nil {
			info.Title = title
		}
		if length, err := metadata.MPRISLength(); err == nil && length > 0 {
			info.Duration = float64(length) / 1e6
		}
		if trackID, err := metadata.MPRISTrackID(); err == nil {
			info.TrackID = trackID
		}
	}
	if pos, err := p.Position(); err == nil && pos >= 0 {
		info.Position = float64(pos) / 1e6
	}
	return info
}

func (m *MPRISMonitor) Start(stateChan chan<- map[string]PlayerInfo) {
	if err := m.conn.AddMatchSignal(
		dbus.WithMatchObjectPath(DBusPath),
		dbus.WithMatchInterface("org.freedesktop.DBus.Properties"),
		dbus.WithMatchMember("PropertiesChanged"),
	); err != nil {
		slog.Warn("MPRIS AddMatchSignal PropertiesChanged error", "error", err)
	}
	if err := m.conn.AddMatchSignal(
		dbus.WithMatchObjectPath(DBusPath),
		dbus.WithMatchInterface(DBusIface),
		dbus.WithMatchMember("Seeked"),
	); err != nil {
		slog.Warn("MPRIS AddMatchSignal Seeked error", "error", err)
	}
	if err := m.conn.AddMatchSignal(
		dbus.WithMatchObjectPath("/org/freedesktop/DBus"),
		dbus.WithMatchInterface("org.freedesktop.DBus"),
		dbus.WithMatchSender("org.freedesktop.DBus"),
		dbus.WithMatchMember("NameOwnerChanged"),
	); err != nil {
		slog.Warn("MPRIS AddMatchSignal NameOwnerChanged error", "error", err)
	}

	dbusMessages := make(chan *dbus.Signal, 10)
	m.conn.Signal(dbusMessages)

	dbusNames := make(map[string]string)
	allPlayers := make(map[string]PlayerInfo)

	getPlayerNames := func() {
		var names []string
		_ = m.conn.BusObject().Call("org.freedesktop.DBus.ListNames", 0).Store(&names)
		names = slices.DeleteFunc(names, func(n string) bool { return !strings.HasPrefix(n, DBusPrefix) })
		clear(dbusNames)
		for _, name := range names {
			var owner string
			_ = m.conn.BusObject().Call("org.freedesktop.DBus.GetNameOwner", 0, name).Store(&owner)
			if owner != "" {
				dbusNames[owner] = name
			}
		}
	}

	updateState := func(name string) bool {
		p := mpris.NewPlayerWithConnection(name, m.conn)
		info := parsePlayerState(p)
		if info == nil {
			if _, ok := allPlayers[name]; ok {
				delete(allPlayers, name)
				return true
			}
		} else {
			info.Player = strings.TrimPrefix(name, DBusPrefix)
			allPlayers[name] = *info
			return true
		}
		return false
	}

	getPlayerNames()
	for _, name := range dbusNames {
		updateState(name)
	}
	stateChan <- maps.Clone(allPlayers)

	call := func(name, method string) {
		m.conn.Object(name, DBusPath).Call(DBusIface+"."+method, 0)
	}

	for {
		select {
		case posSec := <-m.seekChan:
			activeName := FindActivePlayer(allPlayers)
			if activeName == "" {
				continue
			}
			p := mpris.NewPlayerWithConnection(activeName, m.conn)
			info := allPlayers[activeName]
			targetMicros := int64(posSec * 1e6)
			if info.TrackID != "" && info.TrackID != "/" {
				p.SetPosition(info.TrackID, targetMicros)
			} else {
				if curPos, err := p.Position(); err == nil {
					p.SeekTo(targetMicros - curPos)
				}
			}
			if updateState(activeName) {
				stateChan <- maps.Clone(allPlayers)
			}
		case sig := <-dbusMessages:
			if sig.Name == "org.freedesktop.DBus.NameOwnerChanged" {
				getPlayerNames()
				continue
			}
			name, ok := dbusNames[sig.Sender]
			if !ok {
				continue
			}
			if updateState(name) {
				stateChan <- maps.Clone(allPlayers)
			}
		case act := <-m.actionChan:
			activeName := FindActivePlayer(allPlayers)
			if activeName == "" {
				// If nothing active, take any player if available
				for name := range allPlayers {
					activeName = name
					break
				}
			}
			if activeName == "" {
				continue
			}
			switch act {
			case ActionPlay:
				call(activeName, "Play")
			case ActionPause:
				call(activeName, "Pause")
			case ActionStop:
				call(activeName, "Stop")
			case ActionToggle:
				call(activeName, "PlayPause")
			case ActionPrevious:
				call(activeName, "Previous")
			case ActionNext:
				call(activeName, "Next")
			}
		}
	}
}
