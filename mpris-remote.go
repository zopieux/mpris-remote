package main

import (
	"encoding/json"
	"flag"
	"log"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/godbus/dbus/v5"
	pulse "github.com/jfreymuth/pulse/proto"
	"github.com/leberKleber/go-mpris"
	"github.com/tmaxmax/go-sse"
)

const PREFIX = "org.mpris.MediaPlayer2."
const IFACE = PREFIX + "Player"
const PATH = "/org/mpris/MediaPlayer2"

var (
	listenAddr = flag.String("listen", ":8908", "listen address")
	verbose    = flag.Bool("verbose", false, "prints events if true")
)

type playerState struct {
	State  string `json:"state"`
	Title  string `json:"title"`
	Artist string `json:"artist"`
	Player string `json:"player"`
	Volume int    `json:"volume"`
	Mute   bool   `json:"mute"`
}

type playersState = map[string]playerState

func findActivePlayer(players playersState) string {
	rank := func(n string) int {
		s := players[n]
		return map[string]int{"playing": 2, "paused": 1, "stopped": 0}[s.State]
	}
	return slices.MaxFunc(slices.Collect(maps.Keys(players)), func(a, b string) int {
		return rank(a) - rank(b)
	})
}

func publish(data interface{}, serv *sse.Server) {
	j, _ := json.Marshal(data)
	msg := &sse.Message{}
	msg.AppendData(string(j))
	serv.Publish(msg)
	if *verbose {
		log.Printf("published: %+v", data)
	}
}

func parsePlayerState(p mpris.Player) *playerState {
	s := playerState{}
	ps, _ := p.PlaybackStatus()
	if ps == "" {
		return nil
	}
	m, _ := p.Metadata()
	switch ps {
	case mpris.PlaybackStatusPlaying:
		s.State = "playing"
	case mpris.PlaybackStatusPaused:
		s.State = "paused"
	case mpris.PlaybackStatusStopped:
		return nil
	}
	if artists, err := m.XESAMArtist(); err == nil && len(artists) > 0 {
		s.Artist = strings.Join(artists, ", ")
	}
	if title, err := m.XESAMTitle(); err == nil {
		s.Title = title
	}
	return &s
}

type actionPlay struct{ name string }
type actionPause struct{ name string }
type actionStop struct{ name string }
type actionPrevious struct{ name string }
type actionNext struct{ name string }

func mprisEvents(conn *dbus.Conn, stateChan chan<- playersState, actChan <-chan interface{}) {
	if err := conn.AddMatchSignal(
		dbus.WithMatchObjectPath(PATH),
		dbus.WithMatchInterface("org.freedesktop.DBus.Properties"),
		dbus.WithMatchMember("PropertiesChanged"),
	); err != nil {
		log.Fatalln(err)
	}
	if err := conn.AddMatchSignal(
		dbus.WithMatchObjectPath("/org/freedesktop/DBus"),
		dbus.WithMatchInterface("org.freedesktop.DBus"),
		dbus.WithMatchSender("org.freedesktop.DBus"),
		dbus.WithMatchMember("NameOwnerChanged"),
	); err != nil {
		log.Fatalln(err)
	}
	dbusMessages := make(chan *dbus.Signal, 1)
	conn.Signal(dbusMessages)

	dbusNames := map[string]string{}
	allPlayers := map[string]playerState{}

	getPlayerNames := func() {
		var names []string
		_ = conn.BusObject().Call("org.freedesktop.DBus.ListNames", 0).Store(&names)
		names = slices.DeleteFunc(names, func(n string) bool { return !strings.HasPrefix(n, PREFIX) })
		clear(dbusNames)
		for _, name := range names {
			owner := ""
			_ = conn.BusObject().Call("org.freedesktop.DBus.GetNameOwner", 0, name).Store(&owner)
			if owner != "" {
				dbusNames[owner] = name
			}
		}
	}

	updateState := func(name string) bool {
		p := mpris.NewPlayerWithConnection(name, conn)
		state := parsePlayerState(p)
		if state == nil {
			if _, ok := allPlayers[name]; ok {
				delete(allPlayers, name)
				return true
			}
		} else {
			state.Player = strings.TrimPrefix(name, PREFIX)
			allPlayers[name] = *state
			return true
		}
		return false
	}

	maintainNames := func(m *dbus.Signal) bool {
		if m.Name != "org.freedesktop.DBus.NameOwnerChanged" {
			return false
		}
		getPlayerNames()
		return true
	}

	getPlayerNames()
	for _, name := range dbusNames {
		updateState(name)
	}
	stateChan <- allPlayers

	call := func(name string, method string) {
		conn.Object(name, PATH).Call(IFACE+"."+method, 0)
	}

	for {
		select {
		case m := <-dbusMessages:
			if maintainNames(m) {
				continue
			}
			name := ""
			var ok bool
			if name, ok = dbusNames[m.Sender]; !ok {
				continue
			}
			if updateState(name) {
				stateChan <- allPlayers
			}
		case a := <-actChan:
			switch a := a.(type) {
			case actionPlay:
				call(a.name, "Play")
			case actionPause:
				call(a.name, "Pause")
			case actionStop:
				call(a.name, "Stop")
			case actionPrevious:
				call(a.name, "Previous")
			case actionNext:
				call(a.name, "Next")
			}
		}
	}
}

type volumeMute struct {
	volume int
	mute   bool
}

func volumeEvents(volumeChan chan<- volumeMute, setVolumeChan <-chan int) {
	volumePlease := make(chan struct{}, 1)
	client, conn, err := pulse.Connect("")
	if err != nil {
		log.Fatalln(err)
	}
	client.Callback = func(val interface{}) {
		switch val := val.(type) {
		case *pulse.SubscribeEvent:
			if val.Event.GetType() == pulse.EventChange && val.Event.GetFacility() == pulse.EventSink {
				volumePlease <- struct{}{}
			}
		}
	}
	defer conn.Close()
	if err := client.Request(&pulse.SetClientName{Props: pulse.PropList{}}, nil); err != nil {
		log.Fatalln(err)
	}
	if err := client.Request(&pulse.Subscribe{Mask: pulse.SubscriptionMaskSink}, nil); err != nil {
		log.Fatalln(err)
	}
	volumePlease <- struct{}{}
	const DEFAULT_SINK = "@DEFAULT_SINK@"
	getSinkInfo := func() (pulse.GetSinkInfoReply, error) {
		repl := pulse.GetSinkInfoReply{}
		if err := client.Request(&pulse.GetSinkInfo{SinkIndex: pulse.Undefined, SinkName: DEFAULT_SINK}, &repl); err != nil {
			return repl, err
		}
		return repl, nil
	}
	for {
		select {
		case <-volumePlease:
			repl, err := getSinkInfo()
			if err != nil {
				continue
			}
			var acc int64
			for _, vol := range repl.ChannelVolumes {
				acc += int64(vol)
			}
			acc /= int64(len(repl.ChannelVolumes))
			volumeChan <- volumeMute{
				volume: int(float64(acc) / float64(pulse.VolumeNorm) * 100.0),
				mute:   repl.Mute,
			}
		case vol := <-setVolumeChan:
			if vol == -1 {
				repl, err := getSinkInfo()
				if err != nil {
					continue
				}
				client.Request(&pulse.SetSinkMute{SinkIndex: pulse.Undefined, SinkName: DEFAULT_SINK, Mute: !repl.Mute}, nil)
			} else {
				repl, err := getSinkInfo()
				if err != nil {
					continue
				}
				vol := uint32(float64(vol) * float64(pulse.VolumeNorm) / 100.)
				volumes := pulse.ChannelVolumes{}
				for range repl.ChannelVolumes {
					volumes = append(volumes, vol)
				}
				client.Request(&pulse.SetSinkMute{SinkIndex: pulse.Undefined, SinkName: DEFAULT_SINK, Mute: false}, nil)
				client.Request(&pulse.SetSinkVolume{SinkIndex: pulse.Undefined, SinkName: DEFAULT_SINK, ChannelVolumes: volumes}, nil)
			}
		}
	}
}

func main() {
	flag.Parse()

	conn, err := dbus.SessionBus()
	if err != nil {
		log.Fatalln(err)
	}

	allPlayers := playersState{}
	playerActionChan := make(chan interface{}, 1)
	setVolumeChan := make(chan int, 1)

	logRequest := func(r *http.Request) {
		if *verbose {
			log.Printf("got request: %s", r.URL)
		}
	}

	playerHandler := func(notState string, action func(name string) any) func(w http.ResponseWriter, r *http.Request) {
		return func(w http.ResponseWriter, r *http.Request) {
			logRequest(r)
			relevant := slices.DeleteFunc(slices.Collect(maps.Keys(allPlayers)), func(n string) bool { return allPlayers[n].State == notState })
			if len(relevant) == 0 {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			playerActionChan <- action(relevant[0])
			w.WriteHeader(http.StatusOK)
		}
	}

	http.HandleFunc("/play", playerHandler("playing", func(name string) any { return actionPlay{name: name} }))
	http.HandleFunc("/pause", playerHandler("paused", func(name string) any { return actionPause{name: name} }))
	http.HandleFunc("/stop", playerHandler("stopped", func(name string) any { return actionStop{name: name} }))
	http.HandleFunc("/previous", playerHandler("", func(name string) any { return actionPrevious{name: name} }))
	http.HandleFunc("/next", playerHandler("", func(name string) any { return actionNext{name: name} }))

	http.HandleFunc("/volume", func(w http.ResponseWriter, r *http.Request) {
		logRequest(r)
		vol, err := strconv.Atoi(r.URL.Query().Get("level"))
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if 0 < vol && vol < 100 || vol == -1 {
			setVolumeChan <- vol
		}
		w.WriteHeader(http.StatusOK)
	})

	monitor := &sse.Server{}
	http.Handle("/monitor", monitor)

	go http.ListenAndServe(*listenAddr, nil)

	stateChan := make(chan playersState, 1)
	go mprisEvents(conn, stateChan, playerActionChan)

	volumeChan := make(chan volumeMute, 1)
	go volumeEvents(volumeChan, setVolumeChan)

	state := playerState{}
	for {
		newState := state
		select {
		case players := <-stateChan:
			allPlayers = players
			if len(players) == 0 {
				newState.Player = ""
				newState.Artist = ""
				newState.Title = ""
				newState.State = "stopped"
			} else {
				active := players[findActivePlayer(players)]
				newState.Artist = active.Artist
				newState.Title = active.Title
				newState.Player = active.Player
				newState.State = active.State
			}
		case volume := <-volumeChan:
			newState.Volume = volume.volume
			newState.Mute = volume.mute
		}
		if !reflect.DeepEqual(newState, state) {
			state = newState
			publish(state, monitor)
		}
	}
}
