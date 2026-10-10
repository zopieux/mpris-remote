package main

import (
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/zopieux/gocast"
	"mpris-remote/pkg/player"
)

var (
	portFlag         = flag.Int("port", gocast.DefaultPort, "port for CastV2 TLS server")
	friendlyNameFlag = flag.String("friendly-name", "", "friendly device name (default: MPRIS on <hostname>)")
	modelFlag        = flag.String("model", "MPRIS Remote", "model name advertised to Home Assistant")
	verboseFlag      = flag.Bool("verbose", false, "enable verbose logging (debug level)")
)

func main() {
	flag.Parse()

	logLevel := slog.LevelInfo
	if *verboseFlag {
		logLevel = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})))

	friendlyName := *friendlyNameFlag
	if friendlyName == "" {
		host, err := os.Hostname()
		if err != nil {
			host = "unknown"
		}
		friendlyName = "MPRIS on " + host
	}

	uuidStr := gocast.GenerateStableUUID("mpris-remote-cast")

	mprisMon, err := player.NewMPRISMonitor()
	if err != nil {
		slog.Error("failed to initialize MPRIS monitor", "error", err)
		os.Exit(1)
	}

	pulseMon, err := player.NewPulseMonitor()
	if err != nil {
		slog.Error("failed to initialize PulseAudio monitor", "error", err)
		os.Exit(1)
	}

	cmdHandlers := gocast.CommandHandlers{
		OnPlay:     func() { mprisMon.SendAction(player.ActionPlay) },
		OnPause:    func() { mprisMon.SendAction(player.ActionPause) },
		OnStop:     func() { mprisMon.SendAction(player.ActionStop) },
		OnNext:     func() { mprisMon.SendAction(player.ActionNext) },
		OnPrevious: func() { mprisMon.SendAction(player.ActionPrevious) },
		OnSeek:     func(pos float64) { mprisMon.Seek(pos) },
		OnSetVolume: func(pct int) {
			pulseMon.SetVolume(pct)
		},
		OnSetMute: func(muted bool) {
			pulseMon.SetMute(muted)
		},
	}

	srvConfig := gocast.Config{
		FriendlyName: friendlyName,
		Port:         *portFlag,
		Model:        *modelFlag,
	}

	srv := gocast.NewServer(srvConfig, cmdHandlers)
	if err := srv.Start(); err != nil {
		slog.Error("failed to start CastV2 server", "error", err)
		os.Exit(1)
	}
	defer srv.Close()

	mdnsServer, err := gocast.StartAdvertising(uuidStr, friendlyName, *modelFlag, *portFlag)
	if err != nil {
		slog.Warn("failed to start mDNS advertiser", "error", err)
	} else {
		defer mdnsServer.Shutdown()
	}

	mprisChan := make(chan map[string]player.PlayerInfo, 4)
	go mprisMon.Start(mprisChan)

	volumeChan := make(chan player.VolumeInfo, 4)
	go pulseMon.Start(volumeChan)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	slog.Info("mpris-remote Cast receiver started", "id", uuidStr, "name", friendlyName, "port", *portFlag)

	currentState := player.State{
		State: "stopped",
	}

	for {
		select {
		case sig := <-sigChan:
			slog.Info("shutting down", "signal", sig.String())
			return

		case players := <-mprisChan:
			newState := currentState
			if len(players) == 0 {
				newState.Player = ""
				newState.Artist = ""
				newState.Title = ""
				newState.State = "stopped"
				newState.Position = 0
				newState.Duration = 0
			} else {
				activeKey := player.FindActivePlayer(players)
				active := players[activeKey]
				newState.Player = active.Player
				newState.Artist = active.Artist
				newState.Title = active.Title
				newState.State = active.State
				newState.Position = active.Position
				newState.Duration = active.Duration
			}
			if newState != currentState {
				currentState = newState
				srv.UpdateMedia(gocast.MediaStatus{
					PlayerState: currentState.State,
					Title:       currentState.Title,
					Artist:      currentState.Artist,
					Position:    currentState.Position,
					Duration:    currentState.Duration,
				})
				slog.Debug("player state updated", "state", currentState.State, "player", currentState.Player, "title", currentState.Title, "artist", currentState.Artist, "pos", currentState.Position, "dur", currentState.Duration)
			}

		case vol := <-volumeChan:
			if vol.Volume != currentState.Volume || vol.Mute != currentState.Mute {
				currentState.Volume = vol.Volume
				currentState.Mute = vol.Mute
				srv.UpdateVolume(currentState.Volume, currentState.Mute)
				slog.Debug("volume updated", "volume", currentState.Volume, "mute", currentState.Mute)
			}
		}
	}
}
